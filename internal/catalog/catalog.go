package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"zenbridge/internal/config"
	"zenbridge/internal/upstream"
)

const maxCatalogBytes = 1 << 20

type ModelCheck struct {
	RetryAfterSeconds int       `json:"retry_after_seconds,omitempty"`
	ID                string    `json:"id"`
	Protocol          string    `json:"protocol"`
	Status            string    `json:"status"`
	HTTPStatus        int       `json:"http_status,omitempty"`
	ErrorType         string    `json:"error_type,omitempty"`
	CheckedAt         time.Time `json:"checked_at,omitempty"`
	Verified          bool      `json:"verified"`
	Disabled          bool      `json:"definitive_unavailable,omitempty"`
}
type Report struct {
	RetryAfterSeconds int            `json:"retry_after_seconds,omitempty"`
	AccountID         string         `json:"account_id"`
	Models            []ModelCheck   `json:"models"`
	Counts            map[string]int `json:"counts"`
	Truncated         bool           `json:"truncated"`
}
type catalogEntry struct {
	CredentialTag string `json:"credential_tag"`
	Report        Report `json:"report"`
}
type catalogFile struct {
	Accounts map[string]catalogEntry `json:"accounts"`
}

func Tag(a config.Account) string {
	data, _ := json.Marshal([]any{a.APIKey, a.BaseURL, a.Profile, a.AuthMode, a.ClientVersion, a.ClientName, a.ClientChannel, a.InjectCompatTools})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func readCatalog(path string) (catalogFile, error) {
	c := catalogFile{Accounts: map[string]catalogEntry{}}
	f, e := os.Open(path)
	if os.IsNotExist(e) {
		return c, nil
	}
	if e != nil {
		return c, errors.New("cannot open catalog")
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, maxCatalogBytes+1))
	if e != nil || len(b) > maxCatalogBytes {
		return c, errors.New("cannot read bounded catalog")
	}
	if json.Unmarshal(b, &c) != nil || c.Accounts == nil {
		return catalogFile{}, errors.New("invalid catalog")
	}
	return c, nil
}
func writeCatalog(path string, c catalogFile) error {
	b, e := json.MarshalIndent(c, "", "  ")
	if e != nil || len(b) > maxCatalogBytes {
		return errors.New("catalog exceeds limit")
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".catalog-*")
	if e != nil {
		return errors.New("cannot create catalog")
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e == nil {
		e = os.Rename(name, path)
	}
	if e != nil {
		return errors.New("cannot save catalog")
	}
	return nil
}

func lockCatalog(path string) (func(), error) {
	if path == "" {
		return nil, errors.New("catalog_file must be configured")
	}
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return nil, errors.New("cannot create catalog directory")
	}
	f, e := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, errors.New("cannot open catalog lock")
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil, errors.New("another catalog scan is running")
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
func freeCatalogModel(id string) bool { return id == "big-pickle" || strings.HasSuffix(id, "-free") }
func catalogProtocol(id string) string {
	if strings.HasPrefix(id, "muse-spark") {
		return "responses"
	}
	if strings.Contains(strings.ToLower(id), "systemone") || strings.HasPrefix(strings.ToLower(id), "jev") {
		return "unsupported"
	}
	return "chat"
}
func validCatalogID(id string) bool {
	return id != "" && len(id) <= 256 && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "\r\n\x00")
}
func selectedCatalogAccount(cfg config.Config, id string) (config.Account, error) {
	as, e := config.LoadConfiguredAccounts(cfg)
	if e != nil {
		return config.Account{}, e
	}
	for _, a := range as {
		if a.ID == id {
			if !a.Enabled || a.AuthMode != "key" || config.PlaceholderKey(a.APIKey) {
				return config.Account{}, errors.New("catalog requires an enabled real-key account")
			}
			return a, nil
		}
	}
	return config.Account{}, errors.New("catalog account not found")
}
func catalogFetch(ctx context.Context, client *http.Client, a config.Account) (Report, error) {
	r := Report{AccountID: a.ID, Models: []ModelCheck{}}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, e := upstream.BuildUpstreamOperation(ctx, a, map[string]json.RawMessage{}, upstream.NewSessionID(), "chat")
	if e != nil {
		return r, e
	}
	req.URL, e = url.Parse(a.BaseURL + "/models")
	if e != nil {
		return r, errors.New("invalid catalog URL")
	}
	req.Method = http.MethodGet
	req.Body = nil
	req.GetBody = nil
	req.ContentLength = 0
	resp, e := client.Do(req)
	if e != nil {
		return r, errors.New("catalog discovery request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		r.RetryAfterSeconds = int(upstream.ParseRetryAfter(resp.Header.Get("Retry-After")).Seconds())
		return r, fmt.Errorf("catalog discovery returned HTTP %d", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if e != nil || len(b) > 2<<20 {
		return r, errors.New("catalog discovery body exceeds limit or is unreadable")
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &doc) != nil || doc.Data == nil {
		return r, errors.New("invalid model discovery response")
	}
	seen := map[string]bool{}
	for _, m := range doc.Data {
		if !validCatalogID(m.ID) || !freeCatalogModel(m.ID) || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if len(r.Models) >= 256 {
			r.Truncated = true
			continue
		}
		r.Models = append(r.Models, ModelCheck{ID: m.ID, Protocol: catalogProtocol(m.ID), Status: "discovered"})
	}
	sort.Slice(r.Models, func(i, j int) bool { return r.Models[i].ID < r.Models[j].ID })
	return r, nil
}
func countCatalog(r *Report) {
	r.Counts = map[string]int{}
	for _, m := range r.Models {
		r.Counts[m.Status]++
	}
}
func DiscoverModels(ctx context.Context, cfg config.Config, accountID string) (Report, error) {
	return runCatalog(ctx, cfg, accountID, false)
}
func CheckModels(ctx context.Context, cfg config.Config, accountID string) (Report, error) {
	return runCatalog(ctx, cfg, accountID, true)
}

type Options struct {
	MaxChecks   int
	OnlyStale   bool
	Refresh     bool
	ExpectedTag string
}

func CheckModelsLimited(ctx context.Context, cfg config.Config, id string, maxChecks int, onlyStale bool) (Report, error) {
	if maxChecks < 1 || maxChecks > 32 {
		return Report{}, errors.New("invalid catalog check limit")
	}
	return Run(ctx, cfg, id, true, Options{MaxChecks: maxChecks, OnlyStale: onlyStale})
}
func runCatalog(ctx context.Context, cfg config.Config, id string, check bool) (Report, error) {
	return Run(ctx, cfg, id, check, Options{MaxChecks: 32, Refresh: true})
}
func Run(ctx context.Context, cfg config.Config, id string, check bool, opt Options) (Report, error) {
	a, e := selectedCatalogAccount(cfg, id)
	if e != nil {
		return Report{}, e
	}
	if opt.ExpectedTag != "" && opt.ExpectedTag != Tag(a) {
		return Report{}, errors.New("account revision changed")
	}
	unlock, e := lockCatalog(cfg.CatalogFile)
	if e != nil {
		return Report{}, e
	}
	defer unlock()
	cache, e := readCatalog(cfg.CatalogFile)
	if e != nil {
		return Report{}, e
	}
	client := upstream.NewUpstreamClient()
	defer client.CloseIdleConnections()
	entry, cached := cache.Accounts[id]
	cached = cached && entry.CredentialTag == Tag(a)
	var r Report
	if opt.Refresh || !cached {
		r, e = catalogFetch(ctx, client, a)
		if e != nil {
			return r, e
		}
	} else {
		r = entry.Report
		r.Models = append([]ModelCheck(nil), r.Models...)
		r.Truncated = false
	}
	r.RetryAfterSeconds = 0
	old := map[string]ModelCheck{}
	if entry, ok := cache.Accounts[id]; ok && entry.CredentialTag == Tag(a) {
		for _, m := range entry.Report.Models {
			old[m.ID] = m
		}
	}
	for i := range r.Models {
		if prior, ok := old[r.Models[i].ID]; ok {
			r.Models[i] = prior
		}
	}
	if opt.OnlyStale {
		sort.SliceStable(r.Models, func(i, j int) bool { return r.Models[i].CheckedAt.Before(r.Models[j].CheckedAt) })
	}
	delay := 250 * time.Millisecond
	recheck := 24 * time.Hour
	if a.Discovery != nil {
		delay = time.Duration(a.Discovery.DelayMilliseconds) * time.Millisecond
		recheck = time.Duration(a.Discovery.RecheckAfterSeconds) * time.Second
	}
	probes := 0
	stopped := false
	stopReason := "rate_limit_stop"
	for i := range r.Models {
		m := &r.Models[i]
		if !check {
			continue
		}
		if opt.OnlyStale && !m.CheckedAt.IsZero() {
			age := time.Since(m.CheckedAt)
			if m.Verified && m.Status == "available" && age < recheck {
				continue
			}
			backoff := 15 * time.Minute
			if a.Discovery != nil {
				backoff = time.Duration(a.Discovery.FailureBackoffSeconds) * time.Second
			}
			if m.Status != "available" && age < backoff {
				continue
			}
		}
		if m.Protocol == "unsupported" {
			m.Status = "skipped"
			m.ErrorType = "unsupported_protocol"
			continue
		}
		if probes >= opt.MaxChecks || stopped {
			if opt.OnlyStale {
				r.Truncated = true
				continue
			}
			m.Status = "skipped"
			m.ErrorType = "scan_limit"
			if stopped {
				m.ErrorType = stopReason
			}
			r.Truncated = true
			continue
		}
		if probes > 0 {
			select {
			case <-ctx.Done():
				return r, errors.New("catalog scan canceled")
			case <-time.After(delay):
			}
		}
		probes++
		probeCatalog(ctx, client, a, m)
		if m.RetryAfterSeconds > r.RetryAfterSeconds {
			r.RetryAfterSeconds = m.RetryAfterSeconds
		}
		if m.HTTPStatus == 429 && m.ErrorType != "endpoint_unavailable" {
			stopped = true
		}
		if m.HTTPStatus == 401 || m.HTTPStatus == 403 {
			stopped = true
			stopReason = "access_denied_stop"
		}
	}
	countCatalog(&r)
	cache.Accounts[id] = catalogEntry{CredentialTag: Tag(a), Report: r}
	if e = writeCatalog(cfg.CatalogFile, cache); e != nil {
		return r, e
	}
	return r, nil
}
func probeCatalog(ctx context.Context, client *http.Client, a config.Account, m *ModelCheck) {
	timeout := 45 * time.Second
	if a.Discovery != nil {
		timeout = time.Duration(a.Discovery.ProbeTimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	m.RetryAfterSeconds = 0
	m.CheckedAt = time.Now().UTC()
	model, _ := json.Marshal(m.ID)
	body := map[string]json.RawMessage{"model": model, "stream": json.RawMessage(`false`)}
	if m.Protocol == "responses" {
		body["input"] = json.RawMessage(`"Reply exactly OK"`)
		body["max_output_tokens"] = json.RawMessage(`128`)
	} else {
		body["messages"] = json.RawMessage(`[{"role":"user","content":"Reply exactly OK"}]`)
		body["max_tokens"] = json.RawMessage(`128`)
	}
	req, e := upstream.BuildUpstreamOperation(ctx, a, body, upstream.NewSessionID(), m.Protocol)
	if e != nil {
		m.Status = "error"
		m.ErrorType = "request_build"
		return
	}
	resp, e := client.Do(req)
	if e != nil {
		m.Status = "transient"
		m.ErrorType = "transport"
		return
	}
	defer resp.Body.Close()
	m.HTTPStatus = resp.StatusCode
	m.RetryAfterSeconds = int(upstream.ParseRetryAfter(resp.Header.Get("Retry-After")).Seconds())
	if resp.StatusCode != 200 {
		m.Status = "error"
		m.ErrorType = "upstream_status"
		switch {
		case resp.StatusCode == 429:
			m.Status = "transient"
			m.ErrorType = "rate_limited"
			b, readErr := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
			var doc struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if readErr == nil && len(b) <= 64<<10 && json.Unmarshal(b, &doc) == nil && resp.Header.Get("Retry-After") == "" && doc.Error.Type == "server_error" && doc.Error.Message == "Upstream request failed: Endpoint is unavailable." {
				m.ErrorType = "endpoint_unavailable"
			}
		case resp.StatusCode >= 500:
			m.Status = "transient"
			m.ErrorType = "upstream_unavailable"
		case resp.StatusCode == 404 || resp.StatusCode == 410:
			m.Status = "unavailable"
			m.ErrorType = "model_not_found"
			m.Verified = false
			m.Disabled = true
		case resp.StatusCode == 403:
			m.Status = "error"
			m.ErrorType = "access_denied"
			m.Disabled = false
		case resp.StatusCode == 401:
			m.Status = "unavailable"
			m.ErrorType = "authentication"
			m.Verified = false
			m.Disabled = true
		case resp.StatusCode == 400:
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			var doc struct {
				Error struct {
					Code string `json:"code"`
					Type string `json:"type"`
				} `json:"error"`
			}
			_ = json.Unmarshal(b, &doc)
			if doc.Error.Code == "model_not_found" || doc.Error.Type == "model_not_found" {
				m.Status = "unavailable"
				m.ErrorType = "model_not_found"
				m.Verified = false
				m.Disabled = true
			}
		}
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if ctx.Err() != nil {
		m.Status = "transient"
		m.ErrorType = "timeout"
		return
	}
	m.Status = "available"
	m.ErrorType = ""
	m.Verified = true
	m.Disabled = false
}

func Apply(cfg config.Config, accounts []config.Account) []config.Account {
	out := append([]config.Account(nil), accounts...)
	cache, e := readCatalog(cfg.CatalogFile)
	if e != nil {
		return out
	}
	for i := range out {
		a := &out[i]
		entry, ok := cache.Accounts[a.ID]
		if !ok || entry.CredentialTag != Tag(*a) {
			continue
		}
		disabled := map[string]bool{}
		for _, m := range entry.Report.Models {
			if m.Disabled {
				disabled[m.ID] = true
			}
		}
		manual := a.Models
		a.Models = nil
		for _, m := range manual {
			if !disabled[m] {
				a.Models = append(a.Models, m)
			}
		}
		seen := map[string]bool{}
		for _, m := range a.Models {
			seen[m] = true
		}
		for _, m := range entry.Report.Models {
			if m.Verified && m.Protocol != "unsupported" && freeCatalogModel(m.ID) && validCatalogID(m.ID) && !seen[m.ID] {
				a.Models = append(a.Models, m.ID)
				seen[m.ID] = true
			}
		}
	}
	return out
}
