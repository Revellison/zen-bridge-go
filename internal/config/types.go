package config

type Config struct {
	configSource   string
	inlineAccounts *AccountsFile
	clientToken    string

	BaseModels        []string `json:"base_models"`
	CatalogFile       string   `json:"catalog_file"`
	Listen            string   `json:"listen"`
	LogLevel          string   `json:"log_level"`
	AccountsFile      string   `json:"accounts_file"`
	ReloadSeconds     int      `json:"reload_seconds"`
	AllowHTTPUpstream bool     `json:"allow_http_upstream"`
	AllowedHosts      []string `json:"allowed_hosts"`
}

type Account struct {
	Discovery         *DiscoveryConfig `json:"discovery,omitempty"`
	ID                string           `json:"id"`
	APIKey            string           `json:"api_key"`
	Enabled           bool             `json:"enabled"`
	Models            []string         `json:"models"`
	BaseURL           string           `json:"base_url"`
	Profile           string           `json:"profile"`
	AuthMode          string           `json:"auth_mode"`
	ClientVersion     string           `json:"client_version"`
	ClientName        string           `json:"client_name"`
	ClientChannel     string           `json:"client_channel"`
	InjectCompatTools bool             `json:"inject_compat_tools"`
}

type AccountsFile struct {
	Models   []string         `json:"models,omitempty"`
	Defaults *AccountDefaults `json:"defaults,omitempty"`
	APIKeys  []string         `json:"api_keys,omitempty"`
	Accounts []Account        `json:"accounts,omitempty"`
}

type AccountDefaults struct {
	Enabled           *bool            `json:"enabled,omitempty"`
	Discovery         *DiscoveryConfig `json:"discovery,omitempty"`
	BaseURL           string           `json:"base_url"`
	Profile           string           `json:"profile"`
	AuthMode          string           `json:"auth_mode"`
	ClientVersion     string           `json:"client_version"`
	ClientName        string           `json:"client_name"`
	ClientChannel     string           `json:"client_channel"`
	InjectCompatTools bool             `json:"inject_compat_tools"`
}

type DiscoveryConfig struct {
	Enabled                bool `json:"enabled"`
	CheckEnabled           bool `json:"check_enabled"`
	RefreshIntervalSeconds int  `json:"refresh_interval_seconds"`
	CheckIntervalSeconds   int  `json:"check_interval_seconds"`
	MaxChecksPerRun        int  `json:"max_checks_per_run"`
	ProbeTimeoutSeconds    int  `json:"probe_timeout_seconds"`
	DelayMilliseconds      int  `json:"delay_milliseconds"`
	RecheckAfterSeconds    int  `json:"recheck_after_seconds"`
	FailureBackoffSeconds  int  `json:"failure_backoff_seconds"`
}
