package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
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
			body := `{"model":"mock-model","messages":[{"role":"user","content":"hello"}],"temperature":0.25,"stream":` + fmt.Sprint(stream) + `}`
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

func TestServerBrokenReloadRetainsLastGoodAccounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+integrationKey {
			t.Error("old account key not retained")
		}
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer upstream.Close()
	c := integrationConfig(t)
	c.AccountsFile = filepath.Join(t.TempDir(), "accounts.json")
	u, _ := url.Parse(upstream.URL)
	c.AllowedHosts = []string{u.Hostname()}
	a := integrationAccount(upstream.URL)
	data, err := json.Marshal(config.AccountsFile{Accounts: []config.Account{a}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(c.AccountsFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	s, h := integrationServer(t, c, []config.Account{a})
	if err = s.ReloadAccounts(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(c.AccountsFile, []byte(`{"accounts":[broken`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.ReloadAccounts(); err == nil {
		t.Fatal("broken reload accepted")
	}
	code, _, out := integrationRequest(t, h, "POST", "/v1/chat/completions", integrationChat, integrationToken)
	if code != 200 {
		t.Fatalf("last good unusable %d %s", code, out)
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
