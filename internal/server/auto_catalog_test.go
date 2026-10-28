package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zenbridge/internal/config"
)

func catalogFixture(t *testing.T, h http.HandlerFunc) (config.Config, config.Account) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	cfg := integrationConfig(t)
	cfg.AccountsFile = filepath.Join(t.TempDir(), "accounts.json")
	cfg.AllowedHosts = []string{u.Hostname()}
	cfg.AllowHTTPUpstream = true
	a := config.Account{ID: "one", APIKey: "secret-real-value", Enabled: true, Models: []string{"manual"}, BaseURL: s.URL, Profile: "raw", AuthMode: "key"}
	b, _ := json.Marshal(config.AccountsFile{Accounts: []config.Account{a}})
	if e := os.WriteFile(cfg.AccountsFile, b, 0600); e != nil {
		t.Fatal(e)
	}
	as, e := config.LoadAccounts(cfg.AccountsFile, cfg)
	if e != nil {
		t.Fatal(e)
	}
	return cfg, as[0]
}

func catalogSuccess(w http.ResponseWriter) {
	fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
}

func enableCatalogAuto(t *testing.T, cfg config.Config, a config.Account) config.Account {
	t.Helper()
	a.Discovery = &config.DiscoveryConfig{Enabled: true, CheckEnabled: true, MaxChecksPerRun: 1, ProbeTimeoutSeconds: 30, RefreshIntervalSeconds: 21600, CheckIntervalSeconds: 3600, DelayMilliseconds: 250, RecheckAfterSeconds: 86400, FailureBackoffSeconds: 900}
	b, _ := json.Marshal(config.AccountsFile{Accounts: []config.Account{a}})
	if e := os.WriteFile(cfg.AccountsFile, b, 0600); e != nil {
		t.Fatal(e)
	}
	return a
}
func TestAutoCatalogIdleBoundedAndStale(t *testing.T) {
	discoveries, probes := 0, 0
	cfg, a := catalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			discoveries++
			fmt.Fprint(w, `{"data":[{"id":"a-free"},{"id":"b-free"}]}`)
			return
		}
		probes++
		catalogSuccess(w)
	})
	a = enableCatalogAuto(t, cfg, a)
	s := NewServer(cfg, []config.Account{a}, "token", nil)
	defer s.Close()
	states := map[string]*autoCatalogState{}
	s.autoCatalogTick(context.Background(), time.Now(), states)
	if probes != 1 || discoveries != 1 {
		t.Fatalf("unbounded or no initial scan %d %d", probes, discoveries)
	}
	s.autoCatalogTick(context.Background(), time.Now(), states)
	if probes != 1 {
		t.Fatal("hammering schedule")
	}
	states[a.ID].nextCheck = time.Time{}
	s.autoCatalogTick(context.Background(), time.Now(), states)
	if probes != 2 || discoveries != 1 {
		t.Fatalf("stale cache scan refetched/rechecked wrong: %d %d", probes, discoveries)
	}
	states[a.ID].nextCheck = time.Time{}
	s.autoCatalogTick(context.Background(), time.Now(), states)
	if probes != 2 {
		t.Fatal("fresh verified model rechecked")
	}
	a.Discovery.Enabled = false
	b, _ := json.Marshal(config.AccountsFile{Accounts: []config.Account{a}})
	os.WriteFile(cfg.AccountsFile, b, 0600)
	states[a.ID].nextCheck = time.Time{}
	s.autoCatalogTick(context.Background(), time.Now(), states)
	if probes != 2 || len(states) != 0 {
		t.Fatal("disabled automation not reloaded")
	}
}
func TestAutoCatalogReloadsAccounts(t *testing.T) {
	cfg, a := catalogFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":[]}`) })
	s := NewServer(cfg, []config.Account{a}, "token", nil)
	defer s.Close()
	if models := s.pool.Models(); len(models) != 1 || models[0] != "manual" {
		t.Fatalf("initial pool %v", models)
	}
	second := a
	second.ID = "two"
	second.APIKey = "second-real-key" // the parser rejects a duplicated api_key
	second.Models = []string{"second"}
	document, _ := json.Marshal(config.AccountsFile{Accounts: []config.Account{a, second}})
	if err := os.WriteFile(cfg.AccountsFile, document, 0600); err != nil {
		t.Fatal(err)
	}
	if pinned, _, err := s.pool.Acquire("manual", "conv-1"); err != nil || pinned.ID != a.ID {
		t.Fatalf("pin before reload: %v %v", pinned.ID, err)
	}
	states := map[string]*autoCatalogState{}
	s.autoCatalogTick(context.Background(), time.Now(), states)
	models := s.pool.Models()
	if len(models) != 2 || models[0] != "manual" || models[1] != "second" {
		t.Fatalf("accounts not reloaded: %v", models)
	}
	if pinned, _, err := s.pool.Acquire("manual", "conv-1"); err != nil || pinned.ID != a.ID {
		t.Fatalf("reload dropped a conversation pin: %v %v", pinned.ID, err)
	}
	// A broken configuration must keep the last good account set.
	if err := os.WriteFile(cfg.AccountsFile, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	s.autoCatalogTick(context.Background(), time.Now(), states)
	if models := s.pool.Models(); len(models) != 2 {
		t.Fatalf("broken configuration dropped accounts: %v", models)
	}
}

func TestAutoCatalogQuotaBackoff(t *testing.T) {
	probes := 0
	cfg, a := catalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[{"id":"a-free"},{"id":"b-free"}]}`)
			return
		}
		probes++
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(429)
	})
	a = enableCatalogAuto(t, cfg, a)
	s := NewServer(cfg, []config.Account{a}, "token", nil)
	defer s.Close()
	states := map[string]*autoCatalogState{}
	now := time.Now()
	s.autoCatalogTick(context.Background(), now, states)
	if probes != 1 || states[a.ID].blockedUntil.Before(now.Add(7200*time.Second)) {
		t.Fatal("retry-after ignored")
	}
	states[a.ID].nextCheck = time.Time{}
	s.autoCatalogTick(context.Background(), now.Add(time.Hour), states)
	if probes != 1 {
		t.Fatal("quota retried early")
	}
}
