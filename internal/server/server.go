package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zenbridge/internal/accounts"
	"zenbridge/internal/catalog"
	"zenbridge/internal/config"
	"zenbridge/internal/upstream"
)

type Server struct {
	cfg        config.Config
	pool       *accounts.Pool
	client     *http.Client
	token      string
	logger     *slog.Logger
	sessionsMu sync.Mutex
	sessions   map[string]sessionEntry
	seq        atomic.Uint64
	reloadMu   sync.Mutex
	fileStamp  string
}
type sessionEntry struct {
	id      string
	touched time.Time
}

func NewServer(cfg config.Config, accts []config.Account, token string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{cfg: cfg, pool: accounts.NewPool(catalog.Apply(cfg, accts)), client: upstream.NewUpstreamClient(), token: token, logger: logger, sessions: make(map[string]sessionEntry)}
}
func (s *Server) Close() { s.client.CloseIdleConnections() }
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, 405, "method_not_allowed", "GET required")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.pool.Ready() {
			writeError(w, 503, "no_accounts", "configure an enabled account in config.json")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("/v1/models", s.authorized(s.models))
	mux.HandleFunc("/v1/chat/completions", s.authorized(s.chat))
	mux.HandleFunc("/v1/responses", s.authorized(s.chat))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, 404, "unsupported_endpoint", "supported endpoints: /v1/models, /v1/chat/completions, /v1/responses")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(s.token))
		if s.token == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, 401, "unauthorized", "invalid local bridge token")
			return
		}
		next(w, r)
	}
}
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method_not_allowed", "GET required")
		return
	}
	data := make([]map[string]any, 0)
	set := map[string]bool{}
	for _, m := range s.cfg.BaseModels {
		set[m] = true
	}
	for _, m := range s.pool.Models() {
		set[m] = true
	}
	all := make([]string, 0, len(set))
	for m := range set {
		all = append(all, m)
	}
	sort.Strings(all)
	for _, m := range all {
		data = append(data, map[string]any{"id": m, "object": "model", "owned_by": "zen-bridge"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method_not_allowed", "POST required")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, 400, "invalid_request", "cannot read request body")
		return
	}
	var body map[string]json.RawMessage
	if err = json.Unmarshal(raw, &body); err != nil || body == nil {
		writeError(w, 400, "invalid_json", "expected one JSON object")
		return
	}
	raw = nil
	var model string
	_ = json.Unmarshal(body["model"], &model)
	operation := "chat"
	if r.URL.Path == "/v1/responses" {
		operation = "responses"
	}
	stream := false
	_ = json.Unmarshal(body["stream"], &stream)

	conversation := r.Header.Get("X-Bridge-Session")
	affinity := ""
	if conversation != "" {
		mac := hmac.New(sha256.New, []byte(s.token))
		mac.Write([]byte(model + "\x00" + conversation))
		affinity = hex.EncodeToString(mac.Sum(nil))
	}
	account, release, err := s.pool.Acquire(model, affinity)
	if err != nil {
		writeError(w, 503, "no_accounts", "no enabled upstream account is configured")
		return
	}
	defer release()
	sid, err := s.sessionID(account, affinity)
	if err != nil {
		writeError(w, 500, "session_error", "cannot create session identifier")
		return
	}
	up, err := upstream.BuildUpstreamOperation(r.Context(), account, body, sid, operation)
	if err != nil {
		writeError(w, 400, "request_profile_error", "request is incompatible with configured upstream profile")
		return
	}
	reqID := fmt.Sprintf("zb-%d", s.seq.Add(1))
	w.Header().Set("X-Request-ID", reqID)
	start := time.Now()
	resp, err := s.client.Do(up)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.logger.Warn("upstream request failed", "request_id", reqID, "elapsed_ms", time.Since(start).Milliseconds())
		writeError(w, 502, "upstream_transport", "upstream request failed")
		return
	}
	defer resp.Body.Close()
	logArgs := []any{"request_id", reqID, "status", resp.StatusCode, "model", model, "account", account.ID, "client_stream", stream, "upstream_type", resp.Header.Get("Content-Type"), "elapsed_ms", time.Since(start).Milliseconds()}
	s.logger.Info("upstream response", logArgs...)
	for key, values := range resp.Header {
		switch strings.ToLower(key) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(flushWriter{w}, resp.Body)
	s.logger.Info("upstream body finished", "request_id", reqID, "model", model, "elapsed_ms", time.Since(start).Milliseconds(), "error", err != nil)
}

type flushWriter struct {
	http.ResponseWriter
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil {
		err = http.NewResponseController(w.ResponseWriter).Flush()
	}
	return n, err
}

func (s *Server) sessionID(a config.Account, affinity string) (string, error) {
	if affinity == "" {
		return upstream.NewSessionID(), nil
	}
	sum := sha256.Sum256([]byte(a.APIKey))
	key := a.ID + ":" + hex.EncodeToString(sum[:]) + ":" + affinity
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	now := time.Now()
	for k, v := range s.sessions {
		if now.Sub(v.touched) > 15*time.Minute {
			delete(s.sessions, k)
		}
	}
	if v, ok := s.sessions[key]; ok {
		v.touched = now
		s.sessions[key] = v
		return v.id, nil
	}
	if len(s.sessions) >= 128 {
		var oldest string
		var t time.Time
		for k, v := range s.sessions {
			if oldest == "" || v.touched.Before(t) {
				oldest = k
				t = v.touched
			}
		}
		delete(s.sessions, oldest)
	}
	id := upstream.NewSessionID()
	s.sessions[key] = sessionEntry{id, now}
	return id, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": code, "message": msg}})
}

func (s *Server) ReloadAccounts() error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	st, err := os.Stat(config.AccountSourcePath(s.cfg))
	if err != nil {
		return errors.New("accounts file unavailable")
	}
	stamp := fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size())
	if cs, e := os.Stat(s.cfg.CatalogFile); e == nil {
		stamp += fmt.Sprintf(":%d:%d", cs.ModTime().UnixNano(), cs.Size())
	}
	if stamp == s.fileStamp {
		return nil
	}
	a, err := config.LoadConfiguredAccounts(s.cfg)
	if err != nil {
		return err
	}
	s.pool.Reload(catalog.Apply(s.cfg, a))
	s.fileStamp = stamp
	s.logger.Info("accounts configuration loaded", "ready", s.pool.Ready())
	return nil
}
func (s *Server) WatchAccounts(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(s.cfg.ReloadSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.ReloadAccounts(); err != nil {
				s.logger.Warn("accounts reload rejected; previous configuration retained")
			}
		}
	}
}
