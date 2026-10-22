package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"zenbridge/internal/config"
)

func TestTransportProfiles(t *testing.T) {
	body := map[string]json.RawMessage{"model": json.RawMessage(`"model"`), "stream": json.RawMessage(`false`), "tool_choice": json.RawMessage(`"none"`), "tools": json.RawMessage(`[{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}]`)}
	for _, profile := range []string{"raw", "opencode-v1", "opencode-v2", "zen-free-compat"} {
		t.Run(profile, func(t *testing.T) {
			a := config.Account{BaseURL: "https://example.invalid/v1/", Profile: profile, AuthMode: "key", APIKey: "exact-key", ClientVersion: "1.2.3", ClientName: "cli", ClientChannel: "stable", InjectCompatTools: true}
			req, err := BuildUpstream(context.Background(), a, body, "ses_test")
			if err != nil {
				t.Fatal(err)
			}
			if req.URL.String() != "https://example.invalid/v1/chat/completions" || req.Header.Get("Authorization") != "Bearer exact-key" {
				t.Fatal("bad request")
			}
			b, _ := io.ReadAll(req.Body)
			var out map[string]json.RawMessage
			json.Unmarshal(b, &out)
			if string(out["tool_choice"]) != `"none"` {
				t.Fatal("overwrote tool choice")
			}
			if profile == "zen-free-compat" {
				if string(out["stream"]) != "false" {
					t.Fatal("stream flag must be preserved")
				}
				var tools []json.RawMessage
				json.Unmarshal(out["tools"], &tools)
				if len(tools) != 2 {
					t.Fatal("must preserve bash and add read")
				}
				if req.Header.Get("x-opencode-request") != "" {
					t.Fatal("unexpected request ID")
				}
			} else if string(out["stream"]) != "false" {
				t.Fatal("changed stream")
			}
			if profile == "opencode-v2" {
				for _, h := range []string{"x-opencode-session-id", "x-opencode-session", "X-Session-Id", "x-session-affinity"} {
					if req.Header.Get(h) != "ses_test" {
						t.Fatal(h)
					}
				}
			}
		})
	}
	if string(body["stream"]) != "false" {
		t.Fatal("mutated input")
	}
}
func TestAuthNoFallback(t *testing.T) {
	a := config.Account{BaseURL: "https://example.invalid", Profile: "raw", AuthMode: "key"}
	if _, err := BuildUpstream(context.Background(), a, nil, ""); err == nil {
		t.Fatal("missing key accepted")
	}
	a.AuthMode = "public"
	a.APIKey = "ignored"
	r, e := BuildUpstream(context.Background(), a, nil, "")
	if e != nil || r.Header.Get("Authorization") != "Bearer public" {
		t.Fatal("public must be explicit")
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
	body := map[string]json.RawMessage{"input": json.RawMessage(`"hi"`), "stream": json.RawMessage(`false`), "tools": json.RawMessage(`[{"type":"function","name":"bash","parameters":{"type":"object"}}]`), "tool_choice": json.RawMessage(`"none"`)}
	for _, profile := range []string{"raw", "opencode-v1", "opencode-v2", "zen-free-compat"} {
		a := config.Account{BaseURL: "https://example.invalid/v1/", Profile: profile, AuthMode: "public", InjectCompatTools: true}
		r, err := BuildUpstreamOperation(context.Background(), a, body, "ses_test", "responses")
		if err != nil {
			t.Fatal(err)
		}
		if r.URL.Path != "/v1/responses" {
			t.Fatal(r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		json.Unmarshal(b, &payload)
		if string(payload["input"]) != `"hi"` || string(payload["tool_choice"]) != `"none"` {
			t.Fatal(string(b))
		}
		if profile == "zen-free-compat" {
			if string(payload["stream"]) != "false" {
				t.Fatal("changed stream")
			}
			var list []map[string]json.RawMessage
			json.Unmarshal(payload["tools"], &list)
			if len(list) != 2 || string(list[1]["name"]) != `"read"` || list[1]["function"] != nil {
				t.Fatal(string(b))
			}
		} else if string(payload["stream"]) != "false" {
			t.Fatal("changed stream")
		}
	}
	if _, err := BuildUpstreamOperation(context.Background(), config.Account{}, nil, "", "bad"); err == nil {
		t.Fatal("accepted bad operation")
	}
}
