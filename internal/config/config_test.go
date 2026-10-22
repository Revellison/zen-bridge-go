package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigDefaultsAndStrictValidation(t *testing.T) {
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if c.AccountsFile != "accounts.json" || c.CatalogFile != "data/catalog.json" || c.Listen != "0.0.0.0:18788" || c.LogLevel != "info" || c.ReloadSeconds != 5 || len(c.BaseModels) != 9 || len(c.AllowedHosts) != 1 || c.AllowedHosts[0] != "opencode.ai" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	for _, bad := range []string{
		`{"unknown":true}`,
		`{"listen":"not-an-address"}`,
		`{"log_level":"verbose"}`,
		`{"allowed_hosts":[]}`,
		`{"base_models":["same","same"]}`,
		`{"client_token":"short"}`,
		`{"client_token":"has whitespace"}`,
	} {
		if _, err := LoadConfig(writeConfig(t, bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestClientToken(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `{"client_token":"local-secret-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	token, err := ClientToken(c)
	if err != nil || token != "local-secret-123" {
		t.Fatalf("token %q %v", token, err)
	}
	if _, err := ClientToken(Config{}); err == nil {
		t.Fatal("missing token accepted")
	}
}

func TestSimpleKeysInheritanceAndIsolation(t *testing.T) {
	path := writeConfig(t, `{
		"models":["a-free","b-free"],
		"defaults":{"base_url":"https://opencode.ai/zen/v1","profile":"zen-free-compat"},
		"api_keys":["real-key-aaaa","real-key-bbbb"]
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := LoadConfiguredAccounts(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].ID == accounts[1].ID || !strings.HasPrefix(accounts[0].ID, "key_") || strings.Contains(accounts[0].ID, "real-key") {
		t.Fatalf("generated accounts %+v", accounts)
	}
	for _, a := range accounts {
		if len(a.Models) != 2 || a.Models[0] != "a-free" || a.Profile != "zen-free-compat" {
			t.Fatalf("inheritance %+v", a)
		}
	}
	accounts[0].Models[0] = "mutated"
	again, err := LoadConfiguredAccounts(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Models[0] != "a-free" {
		t.Fatal("shared models mutated across reloads")
	}
	for _, bad := range []string{
		`{"api_keys":["same-real-key","same-real-key"]}`,
		`{"api_keys":["placeholder-key"]}`,
		`{"api_keys":[""]}`,
		`{"defaults":{"auth_mode":"public"},"api_keys":[]}`,
	} {
		p := writeConfig(t, bad)
		c, err := LoadConfig(p)
		if err != nil {
			continue
		}
		if _, err = LoadConfiguredAccounts(c); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestSharedModelsAndExplicitAccounts(t *testing.T) {
	path := writeConfig(t, `{
		"models":["shared"],
		"accounts":[
			{"id":"one","api_key":"real-key-one"},
			{"id":"two","api_key":"real-key-two","models":["own"]},
			{"id":"three","api_key":"real-key-three","models":[]}
		]
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := LoadConfiguredAccounts(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 3 || len(accounts[0].Models) != 1 || accounts[0].Models[0] != "shared" || accounts[1].Models[0] != "own" || len(accounts[2].Models) != 0 {
		t.Fatalf("model inheritance %+v", accounts)
	}
	if accounts[0].AuthMode != "key" || accounts[0].Profile != "opencode-v1" || accounts[0].BaseURL != "https://opencode.ai/zen/v1" {
		t.Fatalf("defaults not normalized %+v", accounts[0])
	}
	for _, bad := range []string{
		`{"accounts":[{"id":"one","api_key":"real-key-one"},{"id":"one","api_key":"real-key-two"}]}`,
		`{"accounts":[{"id":"one","api_key":"real-key-one","unknown":true}]}`,
		`{"accounts":[{"id":"one","enabled":true,"api_key":"placeholder-key"}]}`,
		`{"accounts":[{"id":"one","api_key":"real-key-one","profile":"wrong"}]}`,
		`{"accounts":[{"id":"one","api_key":"real-key-one","auth_mode":"public"}]}`,
		`{"accounts":[{"id":"one","api_key":"real-key-one","base_url":"http://localhost/v1"}]}`,
		`{"accounts":[{"id":"one","api_key":"real-key-one","models":["m","m"]}]}`,
		`{"accounts_file":"accounts.json","api_keys":["real-key-one"]}`,
	} {
		p := writeConfig(t, bad)
		c, err := LoadConfig(p)
		if err != nil {
			continue
		}
		if _, err = LoadConfiguredAccounts(c); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	ok := writeConfig(t, `{"allow_http_upstream":true,"allowed_hosts":["localhost"],"accounts":[{"id":"one","api_key":"real-key-one","base_url":"http://localhost/v1"}]}`)
	c, err := LoadConfig(ok)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err = LoadConfiguredAccounts(c)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("http upstream rejected: %v", err)
	}
}

func TestDiscoveryDefaultsAndBounds(t *testing.T) {
	d := &DiscoveryConfig{}
	if e := normalizeDiscovery(d); e != nil {
		t.Fatal(e)
	}
	if d.MaxChecksPerRun != 3 || d.ProbeTimeoutSeconds != 30 || d.RefreshIntervalSeconds != 21600 || d.CheckIntervalSeconds != 3600 || d.DelayMilliseconds != 1000 || d.RecheckAfterSeconds != 86400 || d.FailureBackoffSeconds != 900 {
		t.Fatalf("defaults %+v", d)
	}
	for _, bad := range []DiscoveryConfig{{MaxChecksPerRun: 33}, {ProbeTimeoutSeconds: 4}, {DelayMilliseconds: 249}, {RefreshIntervalSeconds: 59}, {CheckIntervalSeconds: -1}, {RecheckAfterSeconds: 59}, {FailureBackoffSeconds: 59}} {
		if normalizeDiscovery(&bad) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
