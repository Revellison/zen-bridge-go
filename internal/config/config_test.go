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
	if c.AccountsFile != "accounts.json" || c.CatalogFile != "data/catalog.json" || c.LogLevel != "info" || c.ReloadSeconds != 5 || len(c.BaseModels) != 8 || len(c.AllowedHosts) != 1 || c.AllowedHosts[0] != "opencode.ai" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	for _, bad := range []string{
		`{"unknown":true}`,
		`{"listen":"not-an-address"}`,
		`{"log_level":"verbose"}`,
		`{"reload_seconds":0}`,
		`{"reload_seconds":3601}`,
		`{"allowed_hosts":[]}`,
		`{"base_models":["same","same"]}`,
		`{"client_token":"short"}`,
		`{"client_token":"has whitespace"}`,
	} {
		if _, err := LoadConfig(writeConfig(t, bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	reloaded, err := LoadConfig(writeConfig(t, `{"reload_seconds":30}`))
	if err != nil || reloaded.ReloadSeconds != 30 {
		t.Fatalf("reload_seconds %d %v", reloaded.ReloadSeconds, err)
	}
}

func TestListenAddress(t *testing.T) {
	t.Setenv("ZEN_BRIDGE_LISTEN", "")
	if addr, err := ListenAddress(); err != nil || addr != "127.0.0.1:18788" {
		t.Fatalf("default listen %q %v", addr, err)
	}
	t.Setenv("ZEN_BRIDGE_LISTEN", "0.0.0.0:19000")
	if addr, err := ListenAddress(); err != nil || addr != "0.0.0.0:19000" {
		t.Fatalf("configured listen %q %v", addr, err)
	}
	t.Setenv("ZEN_BRIDGE_LISTEN", "not-an-address")
	if _, err := ListenAddress(); err == nil {
		t.Fatal("invalid listen accepted")
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
		"defaults":{"base_url":"https://opencode.ai/inference/openai/v1","profile":"opencode-v2"},
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
		if len(a.Models) != 2 || a.Models[0] != "a-free" || a.Profile != "opencode-v2" {
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
	if accounts[0].AuthMode != "key" || accounts[0].Profile != "opencode-v2" || accounts[0].BaseURL != "https://opencode.ai/inference/openai/v1" {
		t.Fatalf("defaults not normalized %+v", accounts[0])
	}
	if accounts[0].ClientVersion != "2.1.4" || accounts[0].ClientName != "cli" || accounts[0].ClientChannel != "latest" {
		t.Fatalf("client metadata %+v", accounts[0])
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

func TestAccountProxyValidation(t *testing.T) {
	load := func(proxy string) ([]Account, error) {
		path := writeConfig(t, `{"defaults":{"proxy":"`+proxy+`"},"api_keys":["real-key-proxy"]}`)
		cfg, err := LoadConfig(path)
		if err != nil {
			return nil, err
		}
		return LoadConfiguredAccounts(cfg)
	}
	for _, good := range []string{"socks5://127.0.0.1:1080", "socks5h://user:pass@proxy.example:1080", "SOCKS5://10.0.0.1:9050/"} {
		accounts, err := load(good)
		if err != nil || len(accounts) != 1 {
			t.Fatalf("proxy %q rejected: %v", good, err)
		}
		if accounts[0].Proxy == "" || !strings.HasPrefix(accounts[0].Proxy, "socks5") {
			t.Fatalf("proxy not normalized: %q", accounts[0].Proxy)
		}
	}
	for _, bad := range []string{"http://127.0.0.1:1080", "socks5://127.0.0.1", "socks5://", "socks5://127.0.0.1:0", "socks5://127.0.0.1:70000", "socks5://host:1080/path", "socks5://host:1080?x=1"} {
		if _, err := load(bad); err == nil {
			t.Fatalf("accepted proxy %q", bad)
		}
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
