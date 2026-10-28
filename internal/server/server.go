package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

const (
	// sessionLimit bounds the conversation registry. Eviction is by last use, never by age: a
	// conversation must keep one session id for as long as the client keeps talking.
	sessionLimit = 4096
	// defaultAcceptEncoding is negotiated by the bridge when the client declares nothing. The
	// answer is then decoded here, so the client never receives an encoding it did not ask for.
	defaultAcceptEncoding = "gzip"
	// seedLimit bounds how much of the first user turn is used to identify a conversation.
	seedLimit = 4096
	// errorBodyLimit bounds how much of a refusing upstream body is logged.
	errorBodyLimit = 4096
)

var hopByHop = map[string]bool{"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true}

// conversationHeaders identify a conversation when the client sends one. X-Bridge-Session is
// the documented name; the rest are accepted so common clients work without configuration.
var conversationHeaders = []string{"X-Bridge-Session", "X-Conversation-Id", "Conversation-Id", "Session-Id", "X-Session-Id"}

// parentHeaders identify the parent conversation of a child (subagent) session.
var parentHeaders = []string{"X-Bridge-Parent", "X-Parent-Session"}

type Server struct {
	cfg         config.Config
	pool        *accounts.Pool
	clients     *upstream.Clients
	token       string
	logger      *slog.Logger
	sessions    *sessionRegistry
	reloadMu    sync.Mutex
	accountsRev [32]byte
	seq         atomic.Uint64
}

type sessionEntry struct {
	id      string
	touched time.Time
}

type sessionRegistry struct {
	mu      sync.Mutex
	entries map[string]sessionEntry
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{entries: map[string]sessionEntry{}}
}

func (r *sessionRegistry) id(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if entry, ok := r.entries[key]; ok {
		entry.touched = now
		r.entries[key] = entry
		return entry.id
	}
	if len(r.entries) >= sessionLimit {
		oldestKey := ""
		var oldest time.Time
		for candidate, entry := range r.entries {
			if oldestKey == "" || entry.touched.Before(oldest) {
				oldestKey, oldest = candidate, entry.touched
			}
		}
		delete(r.entries, oldestKey)
	}
	id := upstream.NewSessionID()
	r.entries[key] = sessionEntry{id: id, touched: now}
	return id
}

func NewServer(cfg config.Config, accts []config.Account, token string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	effective := catalog.Apply(cfg, accts)
	return &Server{cfg: cfg, pool: accounts.NewPool(effective), clients: upstream.NewClients(), token: token, logger: logger, sessions: newSessionRegistry(), accountsRev: accountRevision(effective)}
}
func (s *Server) Close() { s.clients.Close() }
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
	operation := "chat"
	if r.URL.Path == "/v1/responses" {
		operation = "responses"
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, 400, "invalid_request", "cannot read request body")
		return
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		writeError(w, 400, "invalid_json", "request body must be one JSON object")
		return
	}
	// The body is parsed only to route the request and to recognize the conversation; the
	// exact bytes above are what goes upstream.
	var meta requestMeta
	if json.Unmarshal(raw, &meta) != nil {
		writeError(w, 400, "invalid_json", "request body must be one JSON object")
		return
	}
	model := meta.model()
	session, pin := s.identity(r, meta)
	encoding, negotiated := acceptEncoding(r)
	account, release, err := s.pool.Acquire(model, pin)
	if err != nil {
		writeError(w, 503, "no_accounts", "configure an enabled account in config.json")
		return
	}
	defer release()
	client, err := s.clients.Get(account.Proxy)
	if err != nil {
		writeError(w, 502, "proxy_error", "account proxy is misconfigured")
		return
	}
	upstreamRequest, err := upstream.BuildUpstreamOperation(r.Context(), account, raw, session, operation, encoding)
	if err != nil {
		writeError(w, 400, "request_profile_error", "request is incompatible with configured upstream profile")
		return
	}
	started := time.Now()
	resp, err := client.Do(upstreamRequest)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.logger.Warn("upstream request failed", "model", model, "account", account.ID, "proxied", account.Proxy != "", "error", err)
		writeError(w, 502, "upstream_transport", "upstream request failed")
		return
	}
	defer resp.Body.Close()
	if negotiated {
		resp.Body = upstream.DecodedBody(resp)
	}
	for key, values := range resp.Header {
		lower := strings.ToLower(key)
		if hopByHop[lower] {
			continue
		}
		if negotiated && (lower == "content-encoding" || lower == "content-length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Request-ID", fmt.Sprintf("zb-%d", s.seq.Add(1)))
	w.WriteHeader(resp.StatusCode)
	s.logger.Info("upstream response", "status", resp.StatusCode, "model", model, "account", account.ID, "session", session.ID, "client_stream", meta.stream(), "upstream_type", resp.Header.Get("Content-Type"), "elapsed_ms", time.Since(started).Milliseconds())
	writer := &flushWriter{w: w}
	var written int64
	var copyErr error
	if resp.StatusCode >= 400 {
		// The refusing body is what explains a rejected request (a free-tier or budget limit
		// names itself there), so it belongs in the log, not only in the client's window.
		head, readErr := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		s.logger.Warn("upstream rejected request", "status", resp.StatusCode, "model", model, "account", account.ID, "retry_after", resp.Header.Get("Retry-After"), "body", strings.TrimSpace(string(head)))
		if len(head) > 0 {
			n, writeErr := writer.Write(head)
			written += int64(n)
			if writeErr != nil {
				copyErr = writeErr
			}
		}
		if copyErr == nil {
			copyErr = readErr
		}
	}
	if copyErr == nil {
		n, err := io.Copy(writer, resp.Body)
		written += n
		copyErr = err
	}
	writer.flush()
	s.logger.Info("upstream body finished", "model", model, "account", account.ID, "bytes", written, "error", copyErr != nil)
}

// identity returns the session the upstream sees and the pin that keeps one account for the
// whole conversation. A client-supplied conversation header wins; otherwise the first user
// turn identifies the conversation, because every later turn of a conversation repeats it.
func (s *Server) identity(r *http.Request, meta requestMeta) (upstream.Session, string) {
	conversation := conversationKey(r, meta)
	if conversation == "" {
		conversation = "one-off:" + upstream.NewSessionID()
	}
	session := upstream.Session{ProjectID: r.Header.Get("X-Bridge-Project")}
	if parent := headerValue(r, parentHeaders); parent != "" {
		session.ID = s.sessions.id("child:" + parent + "\x00" + conversation)
		session.ParentID = s.sessions.id("parent:" + parent)
		return session, "parent:" + parent
	}
	session.ID = s.sessions.id(conversation)
	return session, conversation
}

func conversationKey(r *http.Request, meta requestMeta) string {
	if value := headerValue(r, conversationHeaders); value != "" {
		return "header:" + value
	}
	seed := meta.seed()
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return "seed:" + hex.EncodeToString(sum[:16])
}

func headerValue(r *http.Request, names []string) string {
	for _, name := range names {
		value := strings.TrimSpace(r.Header.Get(name))
		if value == "" || len(value) > 512 || strings.ContainsAny(value, "\r\n\x00") {
			continue
		}
		return value
	}
	return ""
}

// acceptEncoding forwards what the client negotiated, so the upstream sees the same header a
// real client would send and the client never receives an encoding it cannot decode. When the
// client declares nothing, the bridge asks for gzip itself and unwraps the answer.
func acceptEncoding(r *http.Request) (string, bool) {
	if value := strings.TrimSpace(r.Header.Get("Accept-Encoding")); value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\r\n\x00") {
		return value, false
	}
	return defaultAcceptEncoding, true
}

// requestMeta is deliberately permissive: a body that the client encoded differently must
// still pass through, so every field is raw and only the values the bridge needs are decoded.
type requestMeta struct {
	Model    json.RawMessage `json:"model"`
	Stream   json.RawMessage `json:"stream"`
	Messages json.RawMessage `json:"messages"`
	Input    json.RawMessage `json:"input"`
}

func (m requestMeta) model() string {
	var model string
	if json.Unmarshal(m.Model, &model) != nil {
		return ""
	}
	return model
}

func (m requestMeta) stream() bool {
	var stream bool
	return json.Unmarshal(m.Stream, &stream) == nil && stream
}

// seed is the first user turn of the request: a string, the text parts of a content array, or
// the first user item of a Responses API input.
func (m requestMeta) seed() string {
	if len(m.Messages) > 0 {
		var messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m.Messages, &messages) == nil {
			for _, message := range messages {
				if message.Role == "user" {
					return firstText(message.Content, seedLimit, 3)
				}
			}
		}
	}
	return firstText(m.Input, seedLimit, 3)
}

func firstText(raw json.RawMessage, limit, depth int) string {
	if len(raw) == 0 || limit <= 0 || depth <= 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return cut(text, limit)
	}
	var parts []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "", "text", "input_text", "message", "user":
		default:
			continue
		}
		if len(part.Content) > 0 {
			b.WriteString(firstText(part.Content, limit-b.Len(), depth-1))
		} else {
			b.WriteString(part.Text)
		}
		if b.Len() >= limit {
			break
		}
	}
	return cut(b.String(), limit)
}

func cut(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if n > 0 {
		f.flush()
	}
	return n, err
}
func (f *flushWriter) flush() {
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": code, "message": msg}})
}
