package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenbridge/internal/config"
)

func catalogFixture(t *testing.T, h http.HandlerFunc) (config.Config, config.Account) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	d := t.TempDir()
	a := config.Account{ID: "one", APIKey: "secret-real-value", Enabled: true, Models: []string{"manual"}, BaseURL: s.URL, Profile: "raw", AuthMode: "key"}
	b, _ := json.Marshal(config.AccountsFile{Accounts: []config.Account{a}})
	p := filepath.Join(d, "accounts.json")
	if e := os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	cfg := config.Config{AccountsFile: p, CatalogFile: filepath.Join(d, "catalog.json"), AllowedHosts: []string{u.Hostname()}, AllowHTTPUpstream: true}
	as, e := config.LoadAccounts(p, cfg)
	if e != nil {
		t.Fatal(e)
	}
	return cfg, as[0]
}
func catalogSuccess(w http.ResponseWriter) {
	fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
}
func TestCatalogDiscoveryAndVerifiedMerge(t *testing.T) {
	probes := 0
	cfg, a := catalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-real-value" {
			t.Error("wrong authorization")
		}
		if r.URL.Path == "/models" {
			if r.Method != "GET" {
				t.Error("not GET")
			}
			fmt.Fprint(w, `{"data":[{"id":"paid"},{"id":"big-pickle"},{"id":"test-free"},{"id":"test-free"}]}`)
			return
		}
		probes++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "paid" {
			t.Error("paid model probed")
		}
		catalogSuccess(w)
	})
	r, e := DiscoverModels(context.Background(), cfg, a.ID)
	if e != nil || len(r.Models) != 2 || probes != 0 {
		t.Fatalf("discovery %+v %v", r, e)
	}
	if got := Apply(cfg, []config.Account{a}); len(got[0].Models) != 1 {
		t.Fatal("discovered unverified model published")
	}
	r, e = CheckModels(context.Background(), cfg, a.ID)
	if e != nil || r.Counts["available"] != 2 || probes != 2 {
		t.Fatalf("check %+v %v", r, e)
	}
	got := Apply(cfg, []config.Account{a})
	if len(got[0].Models) != 3 || len(a.Models) != 1 {
		t.Fatal("merge or mutation")
	}
	a.APIKey = "rotated-key"
	if got = Apply(cfg, []config.Account{a}); len(got[0].Models) != 1 {
		t.Fatal("rotated key reused verification")
	}
	b, _ := os.ReadFile(cfg.CatalogFile)
	if strings.Contains(string(b), "secret-real-value") || strings.Contains(string(b), "Bearer") {
		t.Fatal("secret in cache")
	}
	st, _ := os.Stat(cfg.CatalogFile)
	if st.Mode().Perm() != 0600 {
		t.Fatal("cache permissions")
	}
}
func TestCatalogTransientPreservesThenGoneRemoves(t *testing.T) {
	status := 200
	cfg, a := catalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[{"id":"test-free"}]}`)
			return
		}
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		catalogSuccess(w)
	})
	for _, s := range []int{200, 429, 503, 410} {
		status = s
		r, e := CheckModels(context.Background(), cfg, a.ID)
		if e != nil {
			t.Fatal(e)
		}
		want := s != 410
		if r.Models[0].Verified != want {
			t.Fatalf("status %d verified %v", s, r.Models[0].Verified)
		}
		got := Apply(cfg, []config.Account{a})
		if (len(got[0].Models) == 2) != want {
			t.Fatalf("status %d wrong publication", s)
		}
	}
}
func TestCatalogAcceptsAny200(t *testing.T) {
	cfg, a := catalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[{"id":"bad-free"}]}`)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	r, e := CheckModels(context.Background(), cfg, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	if !r.Models[0].Verified || r.Models[0].Status != "available" {
		t.Fatal("HTTP200 rejected")
	}
}
func TestCatalogFreeFilter(t *testing.T) {
	for _, s := range []string{"gpt-5", "muse-spark", "free-paid", "not-free-paid"} {
		if freeCatalogModel(s) {
			t.Fatalf("paid model %s", s)
		}
	}
	for _, s := range []string{"big-pickle", "muse-spark-free", "jev-free"} {
		if !freeCatalogModel(s) {
			t.Fatalf("free model %s", s)
		}
	}
	if catalogProtocol("muse-spark-free") != "responses" || catalogProtocol("jev-free") != "unsupported" {
		t.Fatal("protocol selection")
	}
}
func TestCatalogLockAndBound(t *testing.T) {
	p := filepath.Join(t.TempDir(), "catalog.json")
	unlock, e := lockCatalog(p)
	if e != nil {
		t.Fatal(e)
	}
	if u, e := lockCatalog(p); e == nil {
		u()
		t.Fatal("concurrent lock accepted")
	}
	unlock()
	u, e := lockCatalog(p)
	if e != nil {
		t.Fatal(e)
	}
	u()
	os.WriteFile(p, make([]byte, maxCatalogBytes+1), 0600)
	if _, e := readCatalog(p); e == nil {
		t.Fatal("oversized cache accepted")
	}
}
func TestCatalogRateLimitStopsAndManualInvalidationPersists(t *testing.T) {
	status := 410
	probes := 0
	cfg, a := catalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[{"id":"a-free"},{"id":"b-free"}]}`)
			return
		}
		probes++
		w.WriteHeader(status)
	})
	a.Models = []string{"a-free", "manual"}
	if _, e := CheckModels(context.Background(), cfg, a.ID); e != nil {
		t.Fatal(e)
	}
	if got := Apply(cfg, []config.Account{a}); len(got[0].Models) != 1 {
		t.Fatal("manual unavailable retained")
	}
	if _, e := DiscoverModels(context.Background(), cfg, a.ID); e != nil {
		t.Fatal(e)
	}
	if got := Apply(cfg, []config.Account{a}); len(got[0].Models) != 1 {
		t.Fatal("discovery lost invalidation evidence")
	}
	status = 429
	probes = 0
	r, e := CheckModels(context.Background(), cfg, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	if probes != 1 || r.Counts["skipped"] != 1 || !r.Truncated {
		t.Fatalf("did not stop on429 %+v probes%d", r, probes)
	}
}
func TestCatalogResponsesProbe(t *testing.T) {
	cfg, a := catalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[{"id":"muse-spark-1.3-contributor-free"},{"id":"jev-free"}]}`)
			return
		}
		if r.URL.Path != "/responses" {
			t.Error("wrong protocol")
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if b["max_output_tokens"] != float64(128) || b["input"] != "Reply exactly OK" {
			t.Error("wrong response body")
		}
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	r, e := CheckModels(context.Background(), cfg, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	if r.Counts["available"] != 1 || r.Counts["skipped"] != 1 {
		t.Fatalf("responses probe %+v", r)
	}
}
func TestCatalogEndpointUnavailable429ContinuesOnlyKnownError(t *testing.T) {
	for _, tc := range []struct {
		name, body, retry string
		wantProbes        int
		wantType          string
	}{
		{"endpoint", `{"error":{"type":"server_error","message":"Upstream request failed: Endpoint is unavailable."}}`, "", 2, "endpoint_unavailable"},
		{"generic", `{"error":{"type":"server_error","message":"unknown"}}`, "", 1, "rate_limited"},
		{"quota", `{"error":{"type":"FreeUsageLimitError","message":"Upstream request failed: Endpoint is unavailable."}}`, "", 1, "rate_limited"},
		{"retry-after", `{"error":{"type":"server_error","message":"Upstream request failed: Endpoint is unavailable."}}`, "60", 1, "rate_limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes := 0
			cfg, a := catalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/models" {
					fmt.Fprint(w, `{"data":[{"id":"a-free"},{"id":"b-free"}]}`)
					return
				}
				probes++
				if probes == 1 {
					if tc.retry != "" {
						w.Header().Set("Retry-After", tc.retry)
					}
					w.WriteHeader(429)
					fmt.Fprint(w, tc.body)
					return
				}
				catalogSuccess(w)
			})
			r, e := CheckModels(context.Background(), cfg, a.ID)
			if e != nil {
				t.Fatal(e)
			}
			if probes != tc.wantProbes || r.Models[0].ErrorType != tc.wantType || r.Models[0].Status != "transient" {
				t.Fatalf("probes=%d report=%+v", probes, r)
			}
			if tc.wantProbes == 2 && (r.Truncated || r.Counts["available"] != 1) {
				t.Fatalf("subsequent model not checked %+v", r)
			}
		})
	}
}
func TestCatalogDiscoveryHTTPStatusSafe(t *testing.T) {
	cfg, a := catalogFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401); fmt.Fprint(w, "secret-response") })
	_, e := DiscoverModels(context.Background(), cfg, a.ID)
	if e == nil || e.Error() != "catalog discovery returned HTTP 401" {
		t.Fatalf("unsafe or missing status: %v", e)
	}
}
