package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zenbridge/internal/config"
)

const integrationToken = "fake-secret-local-bridge-token-123456789"
const integrationKey = "fake-secret-selected-upstream-key"
const integrationChat = `{"model":"mock-model","messages":[{"role":"user","content":"hello"}]}`

func integrationConfig(t *testing.T) config.Config {
	t.Helper()
	c, err := config.LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseModels = nil
	c.CatalogFile = filepath.Join(t.TempDir(), "catalog.json")
	c.AllowHTTPUpstream = true
	c.AllowedHosts = []string{"127.0.0.1"}
	return c
}
func integrationAccount(base string) config.Account {
	return config.Account{ID: "mock", Enabled: true, AuthMode: "key", APIKey: integrationKey, BaseURL: base + "/v1", Profile: "raw", Models: []string{"mock-model"}}
}
func integrationServer(t *testing.T, cfg config.Config, accounts []config.Account) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServer(cfg, accounts, integrationToken, nil)
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() { h.Close(); s.Close() })
	return s, h
}
func integrationRequest(t *testing.T, h *httptest.Server, method, path, body, token string) (int, http.Header, string) {
	t.Helper()
	r, err := http.NewRequest(method, h.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := h.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(b)
}

func TestServerHealthAuthReadinessAndModels(t *testing.T) {
	c := integrationConfig(t)
	a := integrationAccount("http://127.0.0.1:1")
	a.Enabled = false
	s, h := integrationServer(t, c, []config.Account{a})
	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{"/healthz", "", 200}, {"/readyz", "", 503}, {"/v1/models", "", 401}, {"/v1/models", "wrong", 401}, {"/v1/models", integrationToken, 200},
	} {
		code, headers, _ := integrationRequest(t, h, "GET", tc.path, "", tc.token)
		if code != tc.want {
			t.Errorf("%s status %d want %d", tc.path, code, tc.want)
		}
		if headers.Get("Cache-Control") != "no-store" {
			t.Error("missing no-store")
		}
	}
	a.Enabled = true
	a.Models = []string{"z-model", "a-model"}
	b := a
	b.ID = "other"
	b.Models = []string{"a-model"}
	s.pool.Reload([]config.Account{a, b})
	code, _, body := integrationRequest(t, h, "GET", "/v1/models", "", integrationToken)
	var result struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if code != 200 || result.Object != "list" || len(result.Data) != 2 || result.Data[0].ID != "a-model" || result.Data[1].ID != "z-model" {
		t.Fatalf("models: %d %s", code, body)
	}
	code, _, _ = integrationRequest(t, h, "GET", "/readyz", "", "")
	if code != 200 {
		t.Fatalf("ready: %d", code)
	}
	a.Models, b.Models = nil, nil
	s.pool.Reload([]config.Account{a, b})
	code, _, _ = integrationRequest(t, h, "GET", "/readyz", "", "")
	if code != 200 {
		t.Fatalf("ready without model list: %d", code)
	}
}

func TestServerPassThroughAndCredentialIsolation(t *testing.T) {
	const rawSSE = "event: weird\ndata: not-json\n\ndata: {\"choices\":[]}\n\n"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			body := `{"model":"mock-model","messages":[{"role":"system","content":"keep <this> & that"},{"role":"user","content":"hello"}],"temperature":0.25,"stream":` + fmt.Sprint(stream) + `}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/chat/completions" || r.Method != "POST" {
					t.Errorf("upstream request %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer "+integrationKey {
					t.Errorf("wrong selected credential: %q", got)
				}
				if r.Header.Get("X-Bridge-Session") != "" {
					t.Error("local session forwarded")
				}
				sent, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if string(sent) != body {
					t.Errorf("body rewritten upstream: %s", sent)
				}
				var payload map[string]any
				if err := json.Unmarshal(sent, &payload); err != nil {
					t.Error(err)
				}
				if payload["stream"] != stream || payload["temperature"] != float64(0.25) {
					t.Errorf("request changed: %#v", payload)
				}
				w.Header().Set("X-Upstream-Test", "kept")
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, rawSSE)
				} else {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"id":"mock","choices":[{"message":{"role":"assistant","content":"hello"}}]}`)
				}
			}))
			defer upstream.Close()
			_, h := integrationServer(t, integrationConfig(t), []config.Account{integrationAccount(upstream.URL)})
			code, headers, out := integrationRequest(t, h, "POST", "/v1/chat/completions", body, integrationToken)
			if code != 200 || calls.Load() != 1 {
				t.Fatalf("response %d %s; calls %d", code, out, calls.Load())
			}
			if headers.Get("X-Upstream-Test") != "kept" {
				t.Fatal("upstream header lost")
			}
			if stream {
				if headers.Get("Content-Type") != "text/event-stream" || out != rawSSE {
					t.Fatalf("stream changed: %q", out)
				}
			} else if !strings.Contains(out, `"content":"hello"`) {
				t.Fatalf("bad JSON %s", out)
			}
		})
	}
}

func TestServerUnlimitedRawStream(t *testing.T) {
	payload := strings.Repeat("x", 300<<10)
	body := "data: " + payload + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
	defer upstream.Close()
	_, h := integrationServer(t, integrationConfig(t), []config.Account{integrationAccount(upstream.URL)})
	code, _, out := integrationRequest(t, h, "POST", "/v1/chat/completions", strings.TrimSuffix(integrationChat, "}")+`,"stream":true}`, integrationToken)
	if code != 200 || out != body {
		t.Fatalf("stream truncated: %d bytes=%d", code, len(out))
	}
}

func TestServerUnknownModelAndSchemaPassthrough(t *testing.T) {
	var seen map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = nil
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	_, h := integrationServer(t, integrationConfig(t), []config.Account{integrationAccount(upstream.URL)})

	body := `{"model":"brand-new-model","messages":[{"role":"wizard","content":[{"type":"image_url","image_url":{"url":"https://example.test/i.png"}}]}],"tools":[` +
		strings.TrimSuffix(strings.Repeat(`{"type":"function","function":{"name":"t","parameters":{}}},`, 80), ",") + `],"stream":"yes"}`
	code, _, out := integrationRequest(t, h, "POST", "/v1/chat/completions", body, integrationToken)
	if code != 200 || out != `{"ok":true}` {
		t.Fatalf("chat %d %s", code, out)
	}
	if seen["model"] != "brand-new-model" || seen["stream"] != "yes" {
		t.Fatalf("payload changed: %#v", seen)
	}
	msgs, _ := seen["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages changed: %#v", seen["messages"])
	}
	tools, _ := seen["tools"].([]any)
	if len(tools) != 80 {
		t.Fatalf("tools changed: %d", len(tools))
	}

	input := `{"model":"brand-new-model","input":"hi","previous_response_id":"resp_x","conversation":"c","store":true,"background":true}`
	code, _, out = integrationRequest(t, h, "POST", "/v1/responses", input, integrationToken)
	if code != 200 || out != `{"ok":true}` {
		t.Fatalf("responses %d %s", code, out)
	}
	if seen["previous_response_id"] != "resp_x" || seen["store"] != true || seen["background"] != true {
		t.Fatalf("responses payload changed: %#v", seen)
	}
}

func TestServerErrorPassthrough(t *testing.T) {
	const upstreamBody = `{"error":{"type":"FreeTierError","message":"secret upstream detail"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(403)
		io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()
	_, h := integrationServer(t, integrationConfig(t), []config.Account{integrationAccount(upstream.URL)})
	code, headers, out := integrationRequest(t, h, "POST", "/v1/chat/completions", integrationChat, integrationToken)
	if code != 403 || out != upstreamBody || headers.Get("Retry-After") != "7" {
		t.Fatalf("error not passed through: %d %q", code, out)
	}
}

func TestServerDoesNotFollowRedirect(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+"/steal", 307) }))
	defer upstream.Close()
	_, h := integrationServer(t, integrationConfig(t), []config.Account{integrationAccount(upstream.URL)})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest("POST", h.URL+"/v1/chat/completions", strings.NewReader(integrationChat))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+integrationToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 307 || targetCalls.Load() != 0 || resp.Header.Get("Location") != target.URL+"/steal" {
		t.Fatalf("redirect status=%d calls=%d location=%q", resp.StatusCode, targetCalls.Load(), resp.Header.Get("Location"))
	}
}

func TestServerStreamingFlushesBeforeUpstreamCompletes(t *testing.T) {
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"early\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	defer close(finish)
	_, h := integrationServer(t, integrationConfig(t), []config.Account{integrationAccount(upstream.URL)})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body := strings.TrimSuffix(integrationChat, "}") + `,"stream":true}`
	req, err := http.NewRequestWithContext(ctx, "POST", h.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+integrationToken)
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatalf("stream headers not flushed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	buf := make([]byte, 128)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("first event not flushed: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "early") {
		t.Fatalf("unexpected first event %q", buf[:n])
	}
}

func TestServerBaseCatalogVisibleWithoutEnabledAccounts(t *testing.T) {
	c := integrationConfig(t)
	c.BaseModels = []string{"base-z", "base-a", "base-a"}
	a := integrationAccount("http://127.0.0.1:1")
	a.Enabled = false
	_, h := integrationServer(t, c, []config.Account{a})
	code, _, out := integrationRequest(t, h, "GET", "/v1/models", "", integrationToken)
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if code != 200 || len(result.Data) != 2 || result.Data[0].ID != "base-a" || result.Data[1].ID != "base-z" {
		t.Fatalf("baseline catalog missing or not sorted/deduplicated: %d %s", code, out)
	}
	code, _, out = integrationRequest(t, h, "GET", "/readyz", "", "")
	if code != 503 {
		t.Fatalf("baseline catalog incorrectly implies readiness: %d %s", code, out)
	}
	body := strings.Replace(integrationChat, "mock-model", "base-a", 1)
	code, _, out = integrationRequest(t, h, "POST", "/v1/chat/completions", body, integrationToken)
	if code != 503 || !strings.Contains(out, "no_accounts") {
		t.Fatalf("request without accounts: %d %s", code, out)
	}
}

type seenRequest struct {
	body, session, affinity, parent, project, client, userAgent, encoding, accept, images string
}

// captureServer records what the upstream sees and answers with a minimal JSON body.
func captureServer(t *testing.T, seen *[]seenRequest) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ := io.ReadAll(r.Body)
		entry := seenRequest{
			body:      string(sent),
			session:   r.Header.Get("x-opencode-session-id"),
			affinity:  r.Header.Get("x-opencode-session"),
			parent:    r.Header.Get("x-opencode-parent-session-id"),
			project:   r.Header.Get("x-opencode-project"),
			client:    r.Header.Get("x-opencode-client"),
			userAgent: r.Header.Get("User-Agent"),
			encoding:  r.Header.Get("Accept-Encoding"),
			accept:    r.Header.Get("Accept"),
			images:    r.Header.Get("TypeSafe-Danger-Zone-Images"),
		}
		mu.Lock()
		*seen = append(*seen, entry)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func postBridge(t *testing.T, h *httptest.Server, body string, headers map[string]string) {
	t.Helper()
	req, err := http.NewRequest("POST", h.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+integrationToken)
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d for %s", resp.StatusCode, body)
	}
}

func TestServerSessionIdentity(t *testing.T) {
	var seen []seenRequest
	upstream := captureServer(t, &seen)
	account := integrationAccount(upstream.URL)
	account.Profile = "opencode-v2"
	account.ClientVersion = "2.1.4"
	account.ClientChannel = "latest"
	account.ClientName = "cli"
	account.Models = []string{"mock-model", "other-model"}
	_, h := integrationServer(t, integrationConfig(t), []config.Account{account})

	first := `{"model":"mock-model","messages":[{"role":"user","content":"first turn"}],"stream":true}`
	postBridge(t, h, first, nil)
	postBridge(t, h, `{"model":"other-model","messages":[{"role":"user","content":"first turn"},{"role":"assistant","content":"x"},{"role":"user","content":"second"}],"stream":true}`, nil)
	postBridge(t, h, `{"model":"mock-model","messages":[{"role":"user","content":"another conversation"}],"stream":true}`, nil)
	postBridge(t, h, first, map[string]string{"X-Bridge-Session": "conversation-a"})
	postBridge(t, h, first, map[string]string{"X-Bridge-Session": "conversation-a"})
	postBridge(t, h, first, map[string]string{"X-Bridge-Parent": "subagent-job", "X-Bridge-Project": "https://github.com/acme/repo.git"})

	if len(seen) != 6 {
		t.Fatalf("upstream calls %d", len(seen))
	}
	if seen[0].session != seen[1].session {
		t.Fatalf("session id changed on model switch: %q %q", seen[0].session, seen[1].session)
	}
	if seen[0].session == seen[2].session {
		t.Fatal("different conversations share a session id")
	}
	if len(seen[0].session) != 30 || !strings.HasPrefix(seen[0].session, "ses_") {
		t.Fatalf("session format %q", seen[0].session)
	}
	if seen[0].affinity != seen[0].session {
		t.Fatal("affinity differs from session for a root session")
	}
	if seen[3].session != seen[4].session {
		t.Fatal("declared conversation does not keep its session id")
	}
	if seen[0].body != first {
		t.Fatalf("body rewritten: %s", seen[0].body)
	}
	if seen[0].project != "global" || seen[0].client != "cli" || seen[0].userAgent != "opencode/latest/2.1.4/cli" {
		t.Fatalf("client headers: project=%q client=%q ua=%q", seen[0].project, seen[0].client, seen[0].userAgent)
	}
	if seen[0].accept != "" || seen[0].images != "" {
		t.Fatalf("injected client headers: accept=%q images=%q", seen[0].accept, seen[0].images)
	}
	if len(seen[0].encoding) == 0 {
		t.Fatal("no accept-encoding negotiated")
	}
	child := seen[5]
	if child.parent == "" || child.affinity != child.parent || child.session == child.parent {
		t.Fatalf("child session headers: %+v", child)
	}
	if len(child.project) != 40 {
		t.Fatalf("project hash %q", child.project)
	}
}

func TestServerPinsAccountPerConversation(t *testing.T) {
	var first, second atomic.Int32
	makeServer := func(calls *atomic.Int32) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			io.WriteString(w, `{"ok":true}`)
		}))
		t.Cleanup(server.Close)
		return server
	}
	one, two := makeServer(&first), makeServer(&second)
	a := integrationAccount(one.URL)
	a.ID = "a"
	b := integrationAccount(two.URL)
	b.ID = "b"
	_, h := integrationServer(t, integrationConfig(t), []config.Account{a, b})
	body := `{"model":"mock-model","messages":[{"role":"user","content":"pinned conversation"}],"stream":true}`
	for i := 0; i < 6; i++ {
		postBridge(t, h, body, nil)
	}
	if first.Load()+second.Load() != 6 {
		t.Fatalf("calls %d/%d", first.Load(), second.Load())
	}
	if first.Load() != 6 && second.Load() != 6 {
		t.Fatalf("account switched inside a conversation: %d/%d", first.Load(), second.Load())
	}
}

func TestServerHonorsAccountProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	account := integrationAccount(upstream.URL)
	account.Proxy = "socks5://127.0.0.1:1"
	_, h := integrationServer(t, integrationConfig(t), []config.Account{account})
	code, _, out := integrationRequest(t, h, "POST", "/v1/chat/completions", integrationChat, integrationToken)
	if code != 502 || !strings.Contains(out, "upstream_transport") {
		t.Fatalf("unreachable proxy did not fail the request: %d %s", code, out)
	}
}

func TestServerDecodesNegotiatedEncoding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Error("bridge did not negotiate compression")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		io.WriteString(writer, `{"ok":true}`)
		writer.Close()
	}))
	defer upstream.Close()
	_, h := integrationServer(t, integrationConfig(t), []config.Account{integrationAccount(upstream.URL)})
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	req, err := http.NewRequest("POST", h.URL+"/v1/chat/completions", strings.NewReader(integrationChat))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+integrationToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil || string(out) != `{"ok":true}` {
		t.Fatalf("body %q err %v", out, err)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatal("compressed body marked as plain")
	}
}
