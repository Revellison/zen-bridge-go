package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"zenbridge/internal/config"
)

func NewUpstreamClient() *http.Client {
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, DisableCompression: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second, ExpectContinueTimeout: time.Second}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var identifierState struct {
	sync.Mutex
	timestamp int64
	counter   uint64
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func NewSessionID() string { return newIdentifier("ses") }
func newIdentifier(prefix string) string {
	identifierState.Lock()
	now := time.Now().UnixMilli()
	if now > identifierState.timestamp {
		identifierState.timestamp = now
		identifierState.counter = 0
	} else {
		now = identifierState.timestamp
	}
	identifierState.counter++
	stamp := uint64(now)*0x1000 + identifierState.counter
	identifierState.Unlock()
	stamp = ^stamp
	var head [12]byte
	const hex = "0123456789abcdef"
	for i := 11; i >= 0; i-- {
		head[i] = hex[stamp&15]
		stamp >>= 4
	}
	var tail [14]byte
	for i := 0; i < len(tail); {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic("secure random source unavailable")
		}
		if b[0] < 248 {
			tail[i] = base62[int(b[0])%62]
			i++
		}
	}
	return prefix + "_" + string(head[:]) + string(tail[:])
}

func BuildUpstream(ctx context.Context, a config.Account, body map[string]json.RawMessage, sessionID string) (*http.Request, error) {
	return BuildUpstreamOperation(ctx, a, body, sessionID, "chat")
}

func BuildUpstreamOperation(ctx context.Context, a config.Account, body map[string]json.RawMessage, sessionID, operation string) (*http.Request, error) {
	path := ""
	switch operation {
	case "chat":
		path = "/chat/completions"
	case "responses":
		path = "/responses"
	default:
		return nil, errors.New("invalid upstream operation")
	}
	payload := make(map[string]json.RawMessage, len(body)+1)
	for k, v := range body {
		payload[k] = v
	}
	var auth string
	switch a.AuthMode {
	case "key":
		if a.APIKey == "" {
			return nil, errors.New("upstream credential missing")
		}
		auth = a.APIKey
	case "public":
		auth = "public"
	default:
		return nil, errors.New("invalid upstream auth mode")
	}
	if a.Profile == "zen-free-compat" && a.InjectCompatTools {
		if err := addCompatToolsOperation(payload, operation); err != nil {
			return nil, err
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("invalid request body")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid upstream URL")
	}
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	switch a.Profile {
	case "raw":
		req.Header.Set("User-Agent", "zenbridge")
	case "opencode-v1":
		req.Header.Set("User-Agent", "opencode/"+a.ClientVersion)
		req.Header.Set("x-opencode-session", sessionID)
		req.Header.Set("x-opencode-request", newIdentifier("msg"))
		req.Header.Set("x-opencode-client", a.ClientName)
	case "opencode-v2":
		req.Header.Set("x-opencode-client", a.ClientName)
		req.Header.Set("User-Agent", fmt.Sprintf("opencode/%s/%s/%s", a.ClientChannel, a.ClientVersion, a.ClientName))
		req.Header.Set("x-opencode-session-id", sessionID)
		req.Header.Set("x-opencode-session", sessionID)
		req.Header.Set("X-Session-Id", sessionID)
		req.Header.Set("x-session-affinity", sessionID)
	case "zen-free-compat":
		version := a.ClientVersion
		if version == "" {
			version = "1.18.31"
		}
		req.Header.Set("User-Agent", "opencode/"+version+" ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14")
		req.Header.Set("x-opencode-session", sessionID)
		req.Header.Set("x-opencode-session-id", sessionID)
	default:
		return nil, errors.New("invalid upstream profile")
	}
	return req, nil
}
func addCompatTools(body map[string]json.RawMessage) error {
	return addCompatToolsOperation(body, "chat")
}
func addCompatToolsOperation(body map[string]json.RawMessage, operation string) error {
	var items []json.RawMessage
	if raw, ok := body["tools"]; ok {
		if err := json.Unmarshal(raw, &items); err != nil {
			return errors.New("invalid tools array")
		}
	}
	names := map[string]bool{}
	for _, raw := range items {
		var t struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			return errors.New("invalid tool")
		}
		if operation == "responses" {
			names[t.Name] = true
		} else {
			names[t.Function.Name] = true
		}
	}
	definitions := []string{`{"type":"function","function":{"name":"bash","description":"Run a shell command using the caller's tool implementation.","parameters":{"type":"object","properties":{"command":{"type":"string"},"description":{"type":"string"},"timeout":{"type":"number"},"workdir":{"type":"string"}},"required":["command","description"]}}}`, `{"type":"function","function":{"name":"read","description":"Read a file using the caller's tool implementation.","parameters":{"type":"object","properties":{"filePath":{"type":"string"},"offset":{"type":"number"},"limit":{"type":"number"}},"required":["filePath"]}}}`}
	for i, name := range []string{"bash", "read"} {
		if !names[name] {
			definition := json.RawMessage(definitions[i])
			if operation == "responses" {
				var nested struct {
					Function map[string]json.RawMessage `json:"function"`
				}
				if json.Unmarshal(definition, &nested) != nil {
					return errors.New("invalid compatibility tool")
				}
				nested.Function["type"] = json.RawMessage(`"function"`)
				definition, _ = json.Marshal(nested.Function)
			}
			items = append(items, definition)
		}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return errors.New("invalid tools")
	}
	body["tools"] = raw
	return nil
}

func ParseRetryAfter(v string) time.Duration {
	if n, e := strconv.Atoi(v); e == nil && n > 0 {
		if n > 86400 {
			n = 86400
		}
		return time.Duration(n) * time.Second
	}
	if t, e := http.ParseTime(v); e == nil {
		d := time.Until(t)
		if d > 24*time.Hour {
			return 24 * time.Hour
		}
		if d > 0 {
			return d
		}
	}
	return 0
}
