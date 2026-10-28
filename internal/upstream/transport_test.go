package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"zenbridge/internal/config"
)

func testAccount(profile string) config.Account {
	return config.Account{BaseURL: "https://example.invalid/v1/", Profile: profile, AuthMode: "key", APIKey: "exact-key", ClientVersion: "2.1.4", ClientName: "cli", ClientChannel: "latest"}
}

func TestTransportProfiles(t *testing.T) {
	body := []byte(`{"model":"model","stream":false,"tool_choice":"none","messages":[{"role":"system","content":"Use <system-reminder> and A&B"}],"tools":[{"type":"function","function":{"name":"shell"}}]}`)
	original := append([]byte(nil), body...)
	for _, profile := range []string{"raw", "opencode-v2"} {
		t.Run(profile, func(t *testing.T) {
			req, err := BuildUpstreamOperation(context.Background(), testAccount(profile), body, Session{ID: "ses_test"}, "chat", "gzip, deflate")
			if err != nil {
				t.Fatal(err)
			}
			if req.URL.String() != "https://example.invalid/v1/chat/completions" || req.Header.Get("Authorization") != "Bearer exact-key" {
				t.Fatal("bad request")
			}
			sent, _ := io.ReadAll(req.Body)
			if !bytes.Equal(sent, original) {
				t.Fatalf("body rewritten: %s", sent)
			}
			if req.Header.Get("Accept-Encoding") != "gzip, deflate" {
				t.Fatalf("accept-encoding lost: %q", req.Header.Get("Accept-Encoding"))
			}
			if req.Header.Get("Accept") != "" {
				t.Fatal("accept header injected")
			}
			if req.Header.Get("TypeSafe-Danger-Zone-Images") != "" {
				t.Fatal("image opt-in header injected")
			}
			if profile == "opencode-v2" {
				for _, header := range []string{"x-opencode-session-id", "x-opencode-session", "X-Session-Id", "x-session-affinity"} {
					if req.Header.Get(header) != "ses_test" {
						t.Fatalf("%s=%q", header, req.Header.Get(header))
					}
				}
				if req.Header.Get("x-opencode-client") != "cli" {
					t.Fatal("missing x-opencode-client")
				}
				if req.Header.Get("x-opencode-project") != "global" {
					t.Fatalf("project %q", req.Header.Get("x-opencode-project"))
				}
				if ua := req.Header.Get("User-Agent"); ua != "opencode/latest/2.1.4/cli" {
					t.Fatalf("bad User-Agent: %s", ua)
				}
			}
			if req.ContentLength != int64(len(original)) {
				t.Fatalf("content length %d", req.ContentLength)
			}
		})
	}
	if !bytes.Equal(body, original) {
		t.Fatal("mutated input")
	}
}

func TestSessionHeadersForChildSessions(t *testing.T) {
	req, err := BuildUpstreamOperation(context.Background(), testAccount("opencode-v2"), []byte(`{}`), Session{ID: "ses_child", ParentID: "ses_parent", ProjectID: "https://github.com/acme/repo.git"}, "chat", "")
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("x-opencode-session-id") != "ses_child" {
		t.Fatal("child session id lost")
	}
	for _, header := range []string{"x-opencode-session", "x-session-affinity", "X-Session-Id"} {
		if req.Header.Get(header) != "ses_parent" {
			t.Fatalf("%s=%q", header, req.Header.Get(header))
		}
	}
	for _, header := range []string{"x-opencode-parent-session-id", "x-parent-session-id"} {
		if req.Header.Get(header) != "ses_parent" {
			t.Fatalf("%s missing", header)
		}
	}
	if got := req.Header.Get("x-opencode-project"); len(got) != 40 || got != ProjectID("https://github.com/acme/repo.git") {
		t.Fatalf("project %q", got)
	}
	if req.Header.Get("Accept-Encoding") != "" {
		t.Fatal("encoding declared without a client request")
	}
}

func TestProjectID(t *testing.T) {
	if got := ProjectID(""); got != "global" {
		t.Fatalf("empty project %q", got)
	}
	hash := "0123456789abcdef0123456789abcdef01234567"
	if got := ProjectID(hash); got != hash {
		t.Fatalf("hash passthrough %q", got)
	}
	first, second := ProjectID("git@github.com:acme/repo.git"), ProjectID("git@github.com:acme/repo.git")
	if first != second || len(first) != 40 || !isLowerHex(first) || first == ProjectID("git@github.com:acme/other.git") {
		t.Fatalf("project derivation %q %q", first, second)
	}
}

func TestCatalogURL(t *testing.T) {
	for base, want := range map[string]string{
		"https://opencode.ai/inference/openai/v1": "https://opencode.ai/inference/v1/models",
		"https://api.example.com/v1/":             "https://api.example.com/v1/models",
		"http://127.0.0.1:8080":                   "http://127.0.0.1:8080/models",
		"https://api.example.com/openai/v1":       "https://api.example.com/v1/models",
	} {
		if got := CatalogURL(base); got != want {
			t.Fatalf("CatalogURL(%q)=%q want %q", base, got, want)
		}
	}
}

func TestCatalogRequestStaysPlain(t *testing.T) {
	req, err := BuildCatalogRequest(context.Background(), testAccount("opencode-v2"), "gzip, deflate")
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodGet || req.URL.String() != "https://example.invalid/v1/models" {
		t.Fatalf("request %s %s", req.Method, req.URL)
	}
	if req.Header.Get("Authorization") != "Bearer exact-key" || req.Header.Get("Accept") != "application/json" {
		t.Fatal("catalog auth headers")
	}
	if req.Header.Get("Content-Type") != "" {
		t.Fatal("content type on GET")
	}
	for _, header := range []string{"x-opencode-session-id", "x-opencode-session", "x-opencode-project", "x-session-affinity", "X-Session-Id"} {
		if req.Header.Get(header) != "" {
			t.Fatalf("conversation header %s on catalog request", header)
		}
	}
	if req.Header.Get("User-Agent") != "opencode/latest/2.1.4/cli" {
		t.Fatalf("ua %q", req.Header.Get("User-Agent"))
	}
}

func TestAuthNoFallback(t *testing.T) {
	a := config.Account{BaseURL: "https://example.invalid", Profile: "raw", AuthMode: "key"}
	if _, err := BuildUpstreamOperation(context.Background(), a, nil, Session{ID: "ses_x"}, "chat", ""); err == nil {
		t.Fatal("missing key accepted")
	}
	a.AuthMode = "invalid"
	if _, err := BuildUpstreamOperation(context.Background(), a, nil, Session{ID: "ses_x"}, "chat", ""); err == nil {
		t.Fatal("invalid auth_mode accepted")
	}
	a.AuthMode, a.APIKey, a.Profile = "key", "key", "invalid"
	if _, err := BuildUpstreamOperation(context.Background(), a, nil, Session{ID: "ses_x"}, "chat", ""); err == nil {
		t.Fatal("invalid profile accepted")
	}
	a.Profile = "raw"
	if _, err := BuildUpstreamOperation(context.Background(), a, nil, Session{}, "chat", ""); err == nil {
		t.Fatal("missing session accepted")
	}
}

func TestClientAndIDs(t *testing.T) {
	c := NewUpstreamClient()
	tr := c.Transport.(*http.Transport)
	if c.Timeout != 0 || tr.MaxConnsPerHost != 0 || tr.ResponseHeaderTimeout != 0 || !tr.DisableCompression {
		t.Fatal("transport configuration")
	}
	if c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirect allowed")
	}
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		s := NewSessionID()
		if len(s) != 30 || !strings.HasPrefix(s, "ses_") || seen[s] {
			t.Fatal("bad ID")
		}
		seen[s] = true
	}
}

func TestResponsesTransport(t *testing.T) {
	body := []byte(`{"input":"hi","stream":false,"tools":[{"type":"function","name":"bash","parameters":{"type":"object"}}],"tool_choice":"none"}`)
	for _, profile := range []string{"raw", "opencode-v2"} {
		r, err := BuildUpstreamOperation(context.Background(), testAccount(profile), body, Session{ID: "ses_test"}, "responses", "gzip")
		if err != nil {
			t.Fatal(err)
		}
		if r.URL.Path != "/v1/responses" {
			t.Fatal(r.URL.Path)
		}
		sent, _ := io.ReadAll(r.Body)
		if !bytes.Equal(sent, body) {
			t.Fatal(string(sent))
		}
	}
	if _, err := BuildUpstreamOperation(context.Background(), config.Account{}, nil, Session{ID: "ses_x"}, "bad", ""); err == nil {
		t.Fatal("accepted bad operation")
	}
}

func TestClientsKeepEgreesSeparate(t *testing.T) {
	clients := NewClients()
	direct, err := clients.Get("")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := clients.Get(""); again != direct {
		t.Fatal("direct client is not reused")
	}
	proxied, err := clients.Get("socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	if proxied == direct {
		t.Fatal("proxy shares the direct client")
	}
	request, _ := http.NewRequest("GET", "https://example.invalid/x", nil)
	got, err := proxied.Transport.(*http.Transport).Proxy(request)
	if err != nil || got == nil || got.Host != "127.0.0.1:1080" {
		t.Fatalf("proxy url %v %v", got, err)
	}
	for _, bad := range []string{"http://127.0.0.1:1080", "socks5://", "://x"} {
		if _, err := clients.Get(bad); err == nil {
			t.Fatalf("accepted proxy %q", bad)
		}
	}
	clients.Close()
}

func TestProxyClientTunnels(t *testing.T) {
	proxy, targets := startSOCKS5(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "through")
	}))
	defer origin.Close()
	client, err := NewClientFor("socks5://" + proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "through" {
		t.Fatalf("body %q", body)
	}
	select {
	case target := <-targets:
		if target != strings.TrimPrefix(origin.URL, "http://") {
			t.Fatalf("proxy dialled %q", target)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not use the proxy")
	}
}

// startSOCKS5 runs a minimal SOCKS5 server that dials whatever CONNECT asks for and then
// copies bytes both ways. It proves a client really leaves through the proxy.
func startSOCKS5(t *testing.T) (string, chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	targets := make(chan string, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = socks5Connect(c, targets)
			}(conn)
		}
	}()
	return listener.Addr().String(), targets
}

func socks5Connect(c net.Conn, targets chan string) error {
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(c, greeting); err != nil {
		return err
	}
	methods := make([]byte, greeting[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return err
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(c, request); err != nil {
		return err
	}
	if request[0] != 5 || request[1] != 1 {
		return errors.New("unsupported socks5 request")
	}
	var host string
	switch request[3] {
	case 1:
		raw := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(c, raw); err != nil {
			return err
		}
		host = net.IP(raw).String()
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(c, length); err != nil {
			return err
		}
		raw := make([]byte, length[0])
		if _, err := io.ReadFull(c, raw); err != nil {
			return err
		}
		host = string(raw)
	case 4:
		raw := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(c, raw); err != nil {
			return err
		}
		host = net.IP(raw).String()
	default:
		return errors.New("unsupported address type")
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(c, port); err != nil {
		return err
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1])))
	select {
	case targets <- target:
	default:
	}
	remote, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return err
	}
	defer remote.Close()
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(remote, c)
		if tcp, ok := remote.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	_, err = io.Copy(c, remote)
	return err
}

func TestDecodedBody(t *testing.T) {
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write([]byte("plain")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	body := io.NopCloser(bytes.NewReader(compressed.Bytes()))
	resp := &http.Response{Header: http.Header{"Content-Encoding": []string{"gzip"}}, Body: body}
	decoded, err := io.ReadAll(DecodedBody(resp))
	if err != nil || string(decoded) != "plain" {
		t.Fatalf("decoded %q %v", decoded, err)
	}
	plain := &http.Response{Header: http.Header{}, Body: io.NopCloser(bytes.NewReader([]byte("as-is")))}
	if got, _ := io.ReadAll(DecodedBody(plain)); string(got) != "as-is" {
		t.Fatalf("identity %q", got)
	}
}
