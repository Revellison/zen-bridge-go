package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode"
)

func readStrictJSON(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("cannot open configuration file")
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return errors.New("cannot read configuration file")
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("configuration must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid configuration JSON or unknown field")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("configuration must contain one JSON object")
	}
	return nil
}

func LoadConfig(path string) (Config, error) {
	c := Config{BaseModels: []string{"big-pickle", "beam-free", "mimo-v2.6-flash-free", "nemotron-3.5-super-vl-free", "nemotron-3.5-lightning-free", "longcat-2.5-preview-free", "deepseek-v4.1-flash-free", "minimax-m3.1-flash-free", "glm-5.3-flash-free"}, CatalogFile: "data/catalog.json", LogLevel: "info", ReloadSeconds: 5, AllowedHosts: []string{"opencode.ai"}}
	if path != "" {
		wire := struct {
			*Config
			ClientToken string          `json:"client_token"`
			Models      json.RawMessage `json:"models"`
			Defaults    json.RawMessage `json:"defaults"`
			APIKeys     json.RawMessage `json:"api_keys"`
			Accounts    json.RawMessage `json:"accounts"`
		}{Config: &c}
		if err := readStrictJSON(path, &wire); err != nil {
			return Config{}, err
		}
		c.configSource = path
		c.clientToken = wire.ClientToken
		if wire.Models != nil || wire.Defaults != nil || wire.APIKeys != nil || wire.Accounts != nil {
			if c.AccountsFile != "" {
				return Config{}, errors.New("inline account settings and accounts_file cannot be combined")
			}
			doc := AccountsFile{}
			for _, field := range []struct {
				name string
				raw  json.RawMessage
				dst  any
			}{
				{"models", wire.Models, &doc.Models}, {"defaults", wire.Defaults, &doc.Defaults}, {"api_keys", wire.APIKeys, &doc.APIKeys}, {"accounts", wire.Accounts, &doc.Accounts},
			} {
				if field.raw == nil {
					continue
				}
				decoder := json.NewDecoder(bytes.NewReader(field.raw))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(field.dst); err != nil {
					return Config{}, fmt.Errorf("invalid %s settings: %w", field.name, err)
				}
			}
			c.inlineAccounts = &doc
		}
	}
	if c.inlineAccounts == nil && c.AccountsFile == "" {
		c.AccountsFile = "accounts.json"
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, errors.New("log_level must be debug, info, warn, or error")
	}
	if t := strings.TrimSpace(c.clientToken); t != "" {
		if len(t) < 8 || len(t) > 256 || strings.ContainsFunc(t, unicode.IsSpace) {
			return Config{}, errors.New("client_token must contain 8-256 visible characters without whitespace")
		}
		c.clientToken = t
	} else {
		c.clientToken = ""
	}
	bounds := []struct {
		name            string
		value, min, max int64
	}{
		{"reload_seconds", int64(c.ReloadSeconds), 1, 3600},
	}
	for _, b := range bounds {
		if b.value < b.min || b.value > b.max {
			return Config{}, fmt.Errorf("%s is outside allowed bounds", b.name)
		}
	}
	if len(c.AllowedHosts) == 0 {
		return Config{}, errors.New("allowed_hosts must contain at least one host")
	}
	for _, h := range c.AllowedHosts {
		if h == "" || strings.TrimSpace(h) != h || strings.ContainsAny(h, "/@?#\\") {
			return Config{}, errors.New("invalid allowed host")
		}
	}
	seenModels := map[string]bool{}
	for _, m := range c.BaseModels {
		if m == "" || strings.TrimSpace(m) != m || strings.ContainsAny(m, "\r\n\x00") || seenModels[m] {
			return Config{}, errors.New("invalid or duplicate base model")
		}
		seenModels[m] = true
	}
	if c.inlineAccounts != nil {
		if _, err := loadAccountsDocument(*c.inlineAccounts, c); err != nil {
			return Config{}, err
		}
	}
	return c, nil
}

var accountIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func PlaceholderKey(k string) bool {
	s := strings.ToLower(strings.TrimSpace(k))
	if s == "" {
		return true
	}
	for _, p := range []string{"placeholder", "your_key", "your-key", "your_api", "your-api", "replace", "changeme", "change-me", "change_me", "example", "<", ">", "${"} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return s == "todo" || s == "xxx" || s == "test" || s == "sk-xxx"
}

func ClientToken(cfg Config) (string, error) {
	if cfg.clientToken == "" {
		return "", errors.New(`set your own API access key in config.json: add "client_token" to the file and restart`)
	}
	return cfg.clientToken, nil
}

func LoadAccounts(path string, cfg Config) ([]Account, error) {
	var doc AccountsFile
	if err := readStrictJSON(path, &doc); err != nil {
		return nil, err
	}
	return loadAccountsDocument(doc, cfg)
}

func AccountSourcePath(cfg Config) string {
	if cfg.inlineAccounts != nil {
		return cfg.configSource
	}
	return cfg.AccountsFile
}

func LoadConfiguredAccounts(cfg Config) ([]Account, error) {
	if cfg.inlineAccounts == nil {
		return LoadAccounts(cfg.AccountsFile, cfg)
	}
	fresh, err := LoadConfig(cfg.configSource)
	if err != nil {
		return nil, err
	}
	if fresh.inlineAccounts == nil {
		return nil, errors.New("changing account source requires restart")
	}
	return loadAccountsDocument(*fresh.inlineAccounts, cfg)
}

func loadAccountsDocument(doc AccountsFile, cfg Config) ([]Account, error) {
	doc.Accounts = append([]Account(nil), doc.Accounts...)
	if len(doc.Accounts) == 0 && doc.APIKeys == nil {
		return nil, errors.New("provide accounts or an api_keys array (which may be empty)")
	}
	if err := validateAccountModels(doc.Models); err != nil {
		return nil, fmt.Errorf("shared models: %s", err)
	}
	defaults := AccountDefaults{}
	if doc.Defaults != nil {
		defaults = *doc.Defaults
	}
	if defaults.AuthMode != "" && defaults.AuthMode != "key" {
		return nil, errors.New("defaults.auth_mode must be key; public requires an explicit account")
	}
	if doc.Defaults != nil || len(doc.APIKeys) > 0 {
		check := accountFromDefaults(defaults)
		check.ID, check.Enabled = "defaults-validation", false
		if _, err := normalizeAccounts([]Account{check}, nil, cfg); err != nil {
			return nil, fmt.Errorf("invalid defaults: %s", err)
		}
	}
	for i, key := range doc.APIKeys {
		if PlaceholderKey(key) {
			return nil, fmt.Errorf("api_keys entry %d requires a non-placeholder key", i+1)
		}
		account := accountFromDefaults(defaults)
		digest := sha256.Sum256([]byte(key))
		account.ID = "key_" + hex.EncodeToString(digest[:16])
		account.APIKey = key
		doc.Accounts = append(doc.Accounts, account)
	}
	return normalizeAccounts(doc.Accounts, doc.Models, cfg)
}

func accountFromDefaults(d AccountDefaults) Account {
	enabled := true
	if d.Enabled != nil {
		enabled = *d.Enabled
	}
	a := Account{Enabled: enabled, BaseURL: d.BaseURL, Profile: d.Profile,
		AuthMode: d.AuthMode, ClientVersion: d.ClientVersion,
		ClientName: d.ClientName, ClientChannel: d.ClientChannel, InjectCompatTools: d.InjectCompatTools}
	if d.Discovery != nil {
		copy := *d.Discovery
		a.Discovery = &copy
	}
	return a
}

func normalizeAccounts(accounts []Account, models []string, cfg Config) ([]Account, error) {
	ids, keys := map[string]bool{}, map[string]bool{}
	allowed := cfg.AllowedHosts
	if len(allowed) == 0 {
		allowed = []string{"opencode.ai"}
	}
	for i := range accounts {
		a := &accounts[i]
		if a.Models == nil && models != nil {
			a.Models = append([]string{}, models...)
		}
		if err := normalizeDiscovery(a.Discovery); err != nil {
			return nil, fmt.Errorf("account entry %d: %s", i+1, err)
		}
		fail := func(msg string) ([]Account, error) { return nil, fmt.Errorf("account entry %d: %s", i+1, msg) }
		if !accountIDPattern.MatchString(a.ID) || ids[a.ID] {
			return fail("invalid or duplicate id")
		}
		ids[a.ID] = true
		if a.Profile == "" {
			a.Profile = "opencode-v1"
		}
		switch a.Profile {
		case "raw", "opencode-v1", "opencode-v2", "zen-free-compat":
		default:
			return fail("invalid profile")
		}
		if a.AuthMode == "" {
			a.AuthMode = "key"
		}
		if a.AuthMode != "key" && a.AuthMode != "public" {
			return fail("invalid auth_mode")
		}
		if a.AuthMode == "public" && a.Profile != "zen-free-compat" {
			return fail("public auth requires zen-free-compat profile")
		}
		if a.Enabled && a.AuthMode == "key" && PlaceholderKey(a.APIKey) {
			return fail("enabled key account requires a non-placeholder api_key")
		}
		if strings.TrimSpace(a.APIKey) != a.APIKey || strings.ContainsAny(a.APIKey, "\r\n\x00") {
			return fail("invalid api_key format")
		}
		if !PlaceholderKey(a.APIKey) {
			if keys[a.APIKey] {
				return fail("duplicate api_key")
			}
			keys[a.APIKey] = true
		}
		if err := validateAccountModels(a.Models); err != nil {
			return fail(err.Error())
		}
		if a.BaseURL == "" {
			a.BaseURL = "https://opencode.ai/zen/v1"
			if a.Profile == "opencode-v2" {
				a.BaseURL = "https://opencode.ai/inference/openai/v1"
			}
		}
		u, err := url.Parse(a.BaseURL)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(a.BaseURL, "#") || u.Opaque != "" {
			return fail("invalid base_url")
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && cfg.AllowHTTPUpstream) {
			return fail("base_url requires https")
		}
		hostOK := false
		for _, h := range allowed {
			if strings.EqualFold(u.Hostname(), h) {
				hostOK = true
			}
		}
		if !hostOK {
			return fail("base_url host is not allowed")
		}
		a.BaseURL = strings.TrimRight(a.BaseURL, "/")
		if a.ClientVersion == "" {
			a.ClientVersion = "1.18.31"
			if a.Profile == "opencode-v2" {
				a.ClientVersion = "2.0.32"
			}
		}
		if a.ClientName == "" {
			a.ClientName = "cli"
		}
		if a.ClientChannel == "" {
			a.ClientChannel = "stable"
		}
		for _, v := range []string{a.ClientVersion, a.ClientName, a.ClientChannel} {
			if strings.ContainsAny(v, "\r\n\x00") {
				return fail("invalid client metadata")
			}
		}
		if a.InjectCompatTools && a.Profile != "zen-free-compat" {
			return fail("inject_compat_tools requires zen-free-compat profile")
		}
	}
	return accounts, nil
}

func normalizeDiscovery(d *DiscoveryConfig) error {
	if d == nil {
		return nil
	}
	fields := []struct {
		p             *int
		def, min, max int
	}{
		{&d.RefreshIntervalSeconds, 21600, 60, 31536000}, {&d.CheckIntervalSeconds, 3600, 60, 31536000},
		{&d.MaxChecksPerRun, 3, 1, 32}, {&d.ProbeTimeoutSeconds, 30, 5, 120}, {&d.DelayMilliseconds, 1000, 250, 60000},
		{&d.RecheckAfterSeconds, 86400, 60, 31536000}, {&d.FailureBackoffSeconds, 900, 60, 31536000},
	}
	for _, f := range fields {
		if *f.p == 0 {
			*f.p = f.def
		}
		if *f.p < f.min || *f.p > f.max {
			return errors.New("discovery setting outside allowed bounds")
		}
	}
	return nil
}

func validateAccountModels(models []string) error {
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		if m == "" || strings.TrimSpace(m) != m || strings.ContainsAny(m, "\r\n\x00") || seen[m] {
			return errors.New("invalid or duplicate model")
		}
		seen[m] = true
	}
	return nil
}
