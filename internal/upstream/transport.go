package upstream

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"zenbridge/internal/config"
)

func newTransport() *http.Transport {
	return &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, DisableCompression: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second, ExpectContinueTimeout: time.Second}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// NewUpstreamClient is the shared direct client: the egress the bridge itself uses when an
// account names no proxy.
func NewUpstreamClient() *http.Client {
	return &http.Client{Transport: newTransport(), CheckRedirect: noRedirect}
}

// NewClientFor returns a client whose requests leave through proxy. The scheme must be
// socks5 or socks5h; the hostname is handed to the proxy instead of being resolved locally,
// which is what makes a proxy useful for changing the gateway's view of the client's region.
func NewClientFor(proxy string) (*http.Client, error) {
	if proxy == "" {
		return NewUpstreamClient(), nil
	}
	u, err := url.Parse(proxy)
	if err != nil || u.Host == "" {
		return nil, errors.New("invalid proxy")
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
	default:
		return nil, errors.New("invalid proxy scheme")
	}
	tr := newTransport()
	tr.Proxy = http.ProxyURL(u)
	return &http.Client{Transport: tr, CheckRedirect: noRedirect}, nil
}

// Clients keeps one client per egress so an account with its own proxy never shares
// connections with the direct egress or with another account.
type Clients struct {
	mu    sync.Mutex
	cache map[string]*http.Client
}

func NewClients() *Clients { return &Clients{cache: map[string]*http.Client{}} }

func (c *Clients) Get(proxy string) (*http.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if client, ok := c.cache[proxy]; ok {
		return client, nil
	}
	client, err := NewClientFor(proxy)
	if err != nil {
		return nil, err
	}
	c.cache[proxy] = client
	return client, nil
}

func (c *Clients) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, client := range c.cache {
		client.CloseIdleConnections()
	}
}

// Session is the identity the OpenCode client puts into request headers. ID is the
// conversation id, ParentID is set for child (subagent) sessions, and ProjectID is a git
// remote, a precomputed project hash, or empty for the global project.
type Session struct {
	ID        string
	ParentID  string
	ProjectID string
}

// Affinity is the value the client sends as x-opencode-session, x-session-affinity and
// X-Session-Id: the parent session when there is one, otherwise the session itself.
func (s Session) Affinity() string {
	if s.ParentID != "" {
		return s.ParentID
	}
	return s.ID
}

// ProjectID mirrors the client: sessions outside a project use the literal "global", and a
// project inside a repository uses the SHA-1 of its git remote. A caller that already has a
// 40-character hash gets it back unchanged.
func ProjectID(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return "global"
	}
	if len(seed) == 40 && isLowerHex(seed) {
		return seed
	}
	sum := sha1.Sum([]byte("git-remote:" + seed))
	return hex.EncodeToString(sum[:])
}

func isLowerHex(v string) bool {
	for i := 0; i < len(v); i++ {
		if (v[i] < '0' || v[i] > '9') && (v[i] < 'a' || v[i] > 'f') {
			return false
		}
	}
	return true
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
	const hexDigits = "0123456789abcdef"
	for i := 11; i >= 0; i-- {
		head[i] = hexDigits[stamp&15]
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

// BuildUpstreamOperation sends body byte for byte: the bridge never re-encodes the request,
// because a JSON round trip reorders keys, re-escapes markup and normalizes numbers, which
// looks nothing like the client's own encoding. acceptEncoding is the encoding the client
// declared, or the one the bridge negotiated for it; it is never rewritten here.
func BuildUpstreamOperation(ctx context.Context, a config.Account, body []byte, s Session, operation, acceptEncoding string) (*http.Request, error) {
	path := ""
	switch operation {
	case "chat":
		path = "/chat/completions"
	case "responses":
		path = "/responses"
	default:
		return nil, errors.New("invalid upstream operation")
	}
	if s.ID == "" {
		return nil, errors.New("upstream session id missing")
	}
	var auth string
	switch a.AuthMode {
	case "key":
		if a.APIKey == "" {
			return nil, errors.New("upstream credential missing")
		}
		auth = a.APIKey
	default:
		return nil, errors.New("invalid upstream auth mode")
	}
	if err := validProfile(a.Profile); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid upstream URL")
	}
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set("Content-Type", "application/json")
	setProfileHeaders(req, a, s, acceptEncoding)
	return req, nil
}

// CatalogURL is where a gateway publishes its model list. OpenCode Zen serves chat and
// responses under /inference/openai/v1 but lists models at /inference/v1/models, so a chat
// base ending in /openai/v1 publishes the list one level up. Any other base is a plain
// OpenAI-compatible root and publishes it beside itself.
func CatalogURL(base string) string {
	trimmed := strings.TrimRight(base, "/")
	if root, ok := strings.CutSuffix(trimmed, "/openai/v1"); ok {
		return root + "/v1/models"
	}
	return trimmed + "/models"
}

// BuildCatalogRequest builds the model listing request used by catalog discovery. It stays
// deliberately plain: the real client never asks the gateway for models and never attaches
// conversation headers to a GET, so none of the session/project headers are sent here.
func BuildCatalogRequest(ctx context.Context, a config.Account, acceptEncoding string) (*http.Request, error) {
	var auth string
	switch a.AuthMode {
	case "key":
		if a.APIKey == "" {
			return nil, errors.New("upstream credential missing")
		}
		auth = a.APIKey
	default:
		return nil, errors.New("invalid upstream auth mode")
	}
	if err := validProfile(a.Profile); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CatalogURL(a.BaseURL), nil)
	if err != nil {
		return nil, errors.New("invalid upstream URL")
	}
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set("Accept", "application/json")
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	req.Header.Set("User-Agent", fmt.Sprintf("opencode/%s/%s/%s", a.ClientChannel, a.ClientVersion, a.ClientName))
	if a.Profile == "raw" {
		req.Header.Set("User-Agent", "zenbridge")
	}
	return req, nil
}

func validProfile(profile string) error {
	switch profile {
	case "raw", "opencode-v2":
		return nil
	default:
		return errors.New("invalid upstream profile")
	}
}

func setProfileHeaders(req *http.Request, a config.Account, s Session, acceptEncoding string) {
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	switch a.Profile {
	case "raw":
		req.Header.Set("User-Agent", "zenbridge")
	case "opencode-v2":
		affinity := s.Affinity()
		req.Header.Set("User-Agent", fmt.Sprintf("opencode/%s/%s/%s", a.ClientChannel, a.ClientVersion, a.ClientName))
		req.Header.Set("x-opencode-session-id", s.ID)
		req.Header.Set("x-opencode-session", affinity)
		req.Header.Set("x-session-affinity", affinity)
		req.Header.Set("X-Session-Id", affinity)
		if s.ParentID != "" {
			req.Header.Set("x-opencode-parent-session-id", s.ParentID)
			req.Header.Set("x-parent-session-id", s.ParentID)
		}
		req.Header.Set("x-opencode-project", ProjectID(s.ProjectID))
		req.Header.Set("x-opencode-client", a.ClientName)
	}
}

// DecodedBody unwraps a response the bridge asked to be compressed for itself. Responses
// negotiated with the client's own Accept-Encoding are forwarded untouched, so they are not
// passed through this function.
func DecodedBody(resp *http.Response) io.ReadCloser {
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "gzip":
		if r, err := gzip.NewReader(resp.Body); err == nil {
			return readCloser{r, resp.Body}
		}
	case "deflate":
		if r, err := zlib.NewReader(resp.Body); err == nil {
			return readCloser{r, resp.Body}
		}
	}
	return resp.Body
}

type readCloser struct {
	io.Reader
	io.Closer
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
