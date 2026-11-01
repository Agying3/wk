package server

// Error captures are deliberately memory-only. The ordinary request log never
// gains prompt bodies. A successful retry does not erase earlier failed attempts.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/upstream"
)

const (
	captureBodyLimit     = 2 << 20
	captureRecordBudget  = 6 << 20
	captureStoreLimit    = 32 << 20
	captureRecordLimit   = 20
	captureInFlightLimit = 8
	captureAttemptLimit  = 8
	captureRetention     = 10 * time.Minute
)

type captureBlob struct {
	raw         []byte          // borrowed for this request only; never serialized or persisted
	Bytes       int             `json:"bytes"`
	SHA256      string          `json:"sha256"`
	HashedBytes int             `json:"hashed_bytes"`
	Truncated   bool            `json:"truncated"`
	Complete    bool            `json:"complete"`
	JSON        json.RawMessage `json:"json,omitempty"`
	Omitted     string          `json:"omitted,omitempty"`
}

type capturedAttempt struct {
	Account         string         `json:"account"`
	Started         time.Time      `json:"started_at"`
	Dispatched      bool           `json:"dispatched"`
	URL             string         `json:"url,omitempty"`
	Proxy           string         `json:"proxy,omitempty"`
	ProxyGroup      string         `json:"proxy_group,omitempty"`
	RequestHeaders  http.Header    `json:"request_headers,omitempty"`
	Request         *captureBlob   `json:"request,omitempty"`
	HTTP            int            `json:"http"`
	BusinessCode    int            `json:"business_code,omitempty"`
	ErrorKind       string         `json:"error_kind,omitempty"`
	Error           *captureBlob   `json:"error,omitempty"`
	StreamErrors    []*captureBlob `json:"stream_errors,omitempty"`
	TransportError  bool           `json:"transport_error,omitempty"`
	ResponseHeaders http.Header    `json:"response_headers,omitempty"`
}

type errorCapture struct {
	ID              string             `json:"id"`
	Created         time.Time          `json:"created_at"`
	Expires         time.Time          `json:"expires_at"`
	Route           string             `json:"route"`
	Method          string             `json:"method"`
	Headers         http.Header        `json:"headers"`
	RequestID       string             `json:"request_id,omitempty"`
	Model           string             `json:"model,omitempty"`
	HTTP            int                `json:"http"`
	InternalStatus  int                `json:"internal_status,omitempty"`
	ErrorCode       string             `json:"error_code,omitempty"`
	MatchedCodes    []int              `json:"matched_codes"`
	Incoming        *captureBlob       `json:"incoming,omitempty"`
	Normalized      *captureBlob       `json:"normalized_chat,omitempty"`
	Attempts        []*capturedAttempt `json:"attempts"`
	AttemptsOmitted int                `json:"attempts_omitted,omitempty"`
	ClientErrors    []*captureBlob     `json:"client_errors,omitempty"`
	ReadIncomplete  bool               `json:"read_incomplete,omitempty"`
	Panicked        bool               `json:"panicked,omitempty"`
}

type captureManager struct {
	mu          sync.Mutex
	initialized bool
	enabled     bool
	codes       []int
	epoch       uint64
	inFlight    int
	skipped     uint64
	evicted     uint64
	bytes       int
	records     []*storedCapture
	timer       *time.Timer
}

type storedCapture struct {
	summary map[string]any
	data    []byte
	expires time.Time
}

func (m *captureManager) initLocked() {
	if !m.initialized {
		m.initialized = true
		m.enabled = true
		m.codes = []int{11128, 11140, 11148}
	}
}

func (m *captureManager) expireLocked(now time.Time) {
	for len(m.records) > 0 && !m.records[0].expires.After(now) {
		m.bytes -= len(m.records[0].data)
		m.records = m.records[1:]
	}
}

func (m *captureManager) scheduleLocked() {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if len(m.records) == 0 {
		return
	}
	m.timer = time.AfterFunc(time.Until(m.records[0].expires), func() { m.mu.Lock(); defer m.mu.Unlock(); m.expireLocked(time.Now()); m.scheduleLocked() })
}

func (m *captureManager) clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.epoch++
	m.records = nil
	m.bytes = 0
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
}

type captureSession struct {
	mu        sync.Mutex
	closed    bool
	manager   *captureManager
	epoch     uint64
	codes     map[int]bool
	secrets   []string // transient, never serialized; known auth values to remove from echoes
	remaining int
	record    errorCapture
}

type captureContextKey struct{}

func captureSessionFrom(ctx context.Context) *captureSession {
	s, _ := ctx.Value(captureContextKey{}).(*captureSession)
	return s
}
func captureAlias(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:10]
}

func (m *captureManager) begin(r *http.Request) *captureSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	if !m.enabled {
		return nil
	}
	if m.inFlight >= captureInFlightLimit {
		m.skipped++
		return nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		m.skipped++
		return nil
	}
	s := &captureSession{manager: m, epoch: m.epoch, codes: map[int]bool{}, remaining: captureRecordBudget}
	for _, code := range m.codes {
		s.codes[code] = true
	}
	s.addSecrets(r.Header)
	for _, c := range r.Cookies() {
		if len(c.Value) >= 6 {
			s.secrets = append(s.secrets, c.Value)
		}
	}
	s.record = errorCapture{ID: "cap_" + hex.EncodeToString(id[:]), Created: time.Now(), Route: r.URL.Path, Method: r.Method, Headers: safeCaptureHeaders(r.Header, s.secrets), Attempts: []*capturedAttempt{}, MatchedCodes: []int{}}
	m.inFlight++
	return s
}

func (s *captureSession) finish() {
	s.mu.Lock()
	s.closed = true
	matched := len(s.record.MatchedCodes) > 0
	var data []byte
	if matched {
		s.remaining = captureRecordBudget
		s.record.Model = safeCaptureText(s.record.Model, s.secrets)
		s.record.RequestID = safeCaptureText(s.record.RequestID, s.secrets)
		s.record.Headers = safeCaptureHeaders(s.record.Headers, s.secrets)
		s.finalize(s.record.Incoming)
		s.finalize(s.record.Normalized)
		for _, a := range s.record.Attempts {
			s.finalize(a.Request)
			s.finalize(a.Error)
			for _, b := range a.StreamErrors {
				s.finalize(b)
			}
		}
		for _, b := range s.record.ClientErrors {
			s.finalize(b)
		}
		sort.Ints(s.record.MatchedCodes)
		s.record.Expires = time.Now().Add(captureRetention)
		data, _ = json.Marshal(s.record)
	}
	s.secrets = nil
	s.mu.Unlock()
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight--
	if !matched || len(data) > captureStoreLimit || !m.enabled || m.epoch != s.epoch {
		return
	}
	m.expireLocked(time.Now())
	for len(m.records) > 0 && (len(m.records) >= captureRecordLimit || m.bytes+len(data) > captureStoreLimit) {
		m.bytes -= len(m.records[0].data)
		m.records = m.records[1:]
		m.evicted++
	}
	summary := map[string]any{"id": s.record.ID, "created_at": s.record.Created, "expires_at": s.record.Expires, "route": s.record.Route, "model": s.record.Model, "request_id": s.record.RequestID, "http": s.record.HTTP, "matched_codes": s.record.MatchedCodes, "attempts": len(s.record.Attempts), "bytes": len(data)}
	m.records = append(m.records, &storedCapture{summary: summary, data: data, expires: s.record.Expires})
	m.bytes += len(data)
	m.scheduleLocked()
}

func (s *captureSession) match(raw []byte) {
	for _, match := range captureCodePattern.FindAllSubmatch(raw, -1) {
		var code int
		_, _ = fmt.Sscanf(string(match[1]), "%d", &code)
		if !s.codes[code] {
			continue
		}
		found := false
		for _, old := range s.record.MatchedCodes {
			if old == code {
				found = true
				break
			}
		}
		if !found {
			s.record.MatchedCodes = append(s.record.MatchedCodes, code)
		}
	}
}

var captureCodePattern = regexp.MustCompile(`(?i)["\\]*\bcode["\\]*\s*:\s*["\\]*(\d{1,8})(?:\D|$)`)
var captureSecretPattern = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=-]+|\bsk-[A-Za-z0-9_-]{8,}|\beyJ[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
var captureURLPattern = regexp.MustCompile(`https?://[^\s<>"'\\]+`)

func safeCaptureText(value string, secrets []string) string {
	for _, secret := range secrets {
		if len(secret) >= 6 {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	value = captureSecretPattern.ReplaceAllString(value, "[REDACTED]")
	return captureURLPattern.ReplaceAllStringFunc(value, func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil {
			return "[URL]"
		}
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	})
}

func (s *captureSession) addSecrets(headers http.Header) {
	for _, name := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key"} {
		value := headers.Get(name)
		if fields := strings.Fields(value); len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
			value = fields[1]
		}
		if len(value) >= 6 {
			s.secrets = append(s.secrets, value)
		}
	}
}

func safeCaptureHeaders(headers http.Header, secrets []string) http.Header {
	out := http.Header{}
	for _, name := range []string{"User-Agent", "Content-Type", "Accept", "Accept-Encoding", "Origin", "Referer", "X-Request-Id", "Request-Id", "X-Upstream-Request-Id", "X-Oneapi-Request-Id", "X-New-Api-Request-Id", "X-Conversation-Id", "X-Request-Trace-Id", "X-Agent-Intent", "X-Agent-Runtime", "X-IDE-Version", "X-Stainless-OS", "X-Stainless-Arch", "X-Stainless-Package-Version", "Traceparent", "Tracestate", "OpenAI-Beta"} {
		if value := headers.Get(name); value != "" {
			if len(value) > 2048 {
				value = value[:2048]
			}
			out.Set(name, safeCaptureText(value, secrets))
		}
	}
	return out
}

func captureRedact(value any, secrets []string, depth int) any {
	if depth > 64 {
		return "[DEPTH_LIMIT]"
	}
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			name := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
			sensitive := name == "authorization" || name == "proxyauthorization" || name == "cookie" || name == "setcookie" || name == "apikey" || name == "accesstoken" || name == "refreshtoken" || name == "idtoken" || name == "password" || name == "passwd" || name == "secret" || name == "clientsecret" || name == "token"
			_, schemaObject := item.(map[string]any)
			if sensitive && !schemaObject && item != nil {
				v[key] = "[REDACTED]"
			} else {
				v[key] = captureRedact(item, secrets, depth+1)
			}
		}
		return v
	case []any:
		for i, item := range v {
			v[i] = captureRedact(item, secrets, depth+1)
		}
		return v
	case string:
		trim := strings.TrimSpace(v)
		if len(trim) > 0 && (trim[0] == '{' || trim[0] == '[') && json.Valid([]byte(trim)) {
			var nested any
			d := json.NewDecoder(strings.NewReader(trim))
			d.UseNumber()
			if d.Decode(&nested) == nil {
				raw, _ := json.Marshal(captureRedact(nested, secrets, depth+1))
				return string(raw)
			}
		}
		if strings.HasPrefix(trim, "data:") && strings.Contains(trim, ";base64,") {
			return "[INLINE_DATA_OMITTED]"
		}
		return safeCaptureText(v, secrets)
	default:
		return value
	}
}

func (s *captureSession) blob(raw []byte, complete bool) *captureBlob {
	h := sha256.Sum256(raw)
	b := &captureBlob{Bytes: len(raw), HashedBytes: len(raw), SHA256: hex.EncodeToString(h[:]), Complete: complete}
	if len(raw) > captureBodyLimit || len(raw) > s.remaining {
		b.Truncated = true
		b.Omitted = "capture_size_limit"
		return b
	}
	// Successful traffic pays for hashing/bounded staging, not JSON redaction.
	// All body slices come from immutable request buffers owned by this request.
	b.raw = raw
	s.remaining -= len(raw)
	return b
}

func (s *captureSession) finalize(b *captureBlob) {
	if b == nil {
		return
	}
	raw := b.raw
	b.raw = nil
	if b.Truncated || !b.Complete || len(raw) == 0 {
		if b.Omitted == "" {
			b.Omitted = "not_complete_json"
		}
		return
	}
	var doc any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if !json.Valid(raw) || d.Decode(&doc) != nil {
		b.Omitted = "not_complete_json"
		return
	}
	clean, err := json.Marshal(captureRedact(doc, s.secrets, 0))
	if err != nil || len(clean) > s.remaining {
		b.Truncated = true
		b.Omitted = "capture_size_limit"
		return
	}
	b.JSON = clean
	s.remaining -= len(clean)
}

type captureReader struct {
	io.ReadCloser
	prefix   bytes.Buffer
	hash     hash.Hash
	count    int
	complete bool
}

func (b *captureReader) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.hash.Write(p[:n])
	b.count += n
	if left := captureBodyLimit - b.prefix.Len(); left > 0 {
		b.prefix.Write(p[:min(left, n)])
	}
	if err == io.EOF {
		b.complete = true
	}
	return n, err
}

type captureWriter struct {
	http.ResponseWriter
	status   int
	prefix   bytes.Buffer
	count    int
	line     []byte
	overlong bool
	errors   [][]byte
}

func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *captureWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *captureWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *captureWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, err := w.ResponseWriter.Write(p)
	w.count += n
	if left := (64 << 10) - w.prefix.Len(); left > 0 {
		w.prefix.Write(p[:min(left, n)])
	}
	for _, b := range p[:n] {
		if b == '\n' {
			if !w.overlong && len(w.errors) < 8 {
				line := bytes.TrimSpace(w.line)
				if bytes.HasPrefix(line, []byte("data:")) {
					raw := bytes.TrimSpace(line[5:])
					var doc map[string]any
					if json.Unmarshal(raw, &doc) == nil && (doc["error"] != nil || upstream.BusinessCode(raw) > 0 || doc["type"] == "error" || doc["type"] == "response.failed" || doc["type"] == "response.incomplete") {
						w.errors = append(w.errors, bytes.Clone(raw))
					}
				}
			}
			w.line = w.line[:0]
			w.overlong = false
		} else if !w.overlong {
			if len(w.line) < 64<<10 {
				w.line = append(w.line, b)
			} else {
				w.line = nil
				w.overlong = true
			}
		}
	}
	return n, err
}

func (h *Handler) observeCapturedRequest(next http.HandlerFunc, w http.ResponseWriter, r *http.Request) {
	s := h.requestCapture.begin(r)
	if s == nil {
		next(w, r)
		return
	}
	reader := &captureReader{ReadCloser: r.Body, hash: sha256.New()}
	if reader.ReadCloser == nil {
		reader.ReadCloser = http.NoBody
	}
	r = r.WithContext(context.WithValue(r.Context(), captureContextKey{}, s))
	r.Body = reader
	writer := &captureWriter{ResponseWriter: w}
	returned := false
	defer func() {
		s.mu.Lock()
		s.record.HTTP = writer.status
		if s.record.HTTP == 0 {
			s.record.HTTP = 200
		}
		s.record.Panicked = !returned
		s.record.ReadIncomplete = !reader.complete
		s.record.Incoming = s.blob(reader.prefix.Bytes(), reader.complete)
		s.record.Incoming.Bytes = reader.count
		s.record.Incoming.HashedBytes = reader.count
		s.record.Incoming.SHA256 = hex.EncodeToString(reader.hash.Sum(nil))
		if reader.count > captureBodyLimit {
			s.record.Incoming.JSON = nil
			s.record.Incoming.Truncated = true
			s.record.Incoming.Omitted = "capture_size_limit"
		}
		if writer.status >= 400 {
			raw := writer.prefix.Bytes()
			s.match(raw)
			b := s.blob(raw, writer.count == len(raw))
			b.Bytes = writer.count
			b.Truncated = b.Truncated || writer.count > len(raw)
			s.record.ClientErrors = append(s.record.ClientErrors, b)
		}
		for _, raw := range writer.errors {
			s.match(raw)
			s.record.ClientErrors = append(s.record.ClientErrors, s.blob(raw, true))
		}
		s.mu.Unlock()
		s.finish()
	}()
	next(writer, r)
	returned = true
}

func captureNormalized(ctx context.Context, body []byte) {
	if s := captureSessionFrom(ctx); s != nil {
		s.mu.Lock()
		s.record.Normalized = s.blob(body, true)
		s.mu.Unlock()
	}
}
func captureChatStat(ctx context.Context, st *chatStat) {
	if s := captureSessionFrom(ctx); s != nil {
		s.mu.Lock()
		s.record.RequestID = st.id[:min(len(st.id), 256)]
		s.record.Model = st.model[:min(len(st.model), 256)]
		s.record.InternalStatus = st.status
		s.record.ErrorCode = st.errorCode[:min(len(st.errorCode), 128)]
		s.mu.Unlock()
	}
}

func captureAttemptContext(ctx context.Context, uid string) context.Context {
	s := captureSessionFrom(ctx)
	if s == nil {
		return ctx
	}
	a := &capturedAttempt{Account: captureAlias(uid), Started: time.Now()}
	s.mu.Lock()
	if len(uid) >= 6 {
		s.secrets = append(s.secrets, uid)
	}
	if len(s.record.Attempts) < captureAttemptLimit {
		s.record.Attempts = append(s.record.Attempts, a)
	} else {
		s.record.AttemptsOmitted++
	}
	s.mu.Unlock()
	return upstream.WithChatObserver(ctx, func(event upstream.ChatObservation) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return
		}
		if event.Phase == "request" {
			s.addSecrets(event.RequestHeaders)
			a.Dispatched = true
			a.URL = safeCaptureText(event.URL, s.secrets)
			a.RequestHeaders = safeCaptureHeaders(event.RequestHeaders, s.secrets)
			a.Request = s.blob(event.Body, true)
		} else if event.Phase == "stream_error" {
			s.match(event.ErrorBody)
			if len(a.StreamErrors) < 8 {
				a.StreamErrors = append(a.StreamErrors, s.blob(event.ErrorBody, true))
			}
			if code := upstream.BusinessCode(event.ErrorBody); code != 0 {
				a.BusinessCode = code
			}
		} else {
			a.HTTP = event.Status
			a.ResponseHeaders = safeCaptureHeaders(event.ResponseHeaders, s.secrets)
			a.TransportError = event.Err != nil
			if len(event.ErrorBody) > 0 {
				s.match(event.ErrorBody)
				a.BusinessCode = upstream.BusinessCode(event.ErrorBody)
				a.ErrorKind = upstream.Classify(event.Status, string(event.ErrorBody)).String()
				a.Error = s.blob(event.ErrorBody, true)
			}
		}
	})
}

func captureProxy(ctx context.Context, uid, proxy, group string) {
	if s := captureSessionFrom(ctx); s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.record.Attempts) > 0 {
			a := s.record.Attempts[len(s.record.Attempts)-1]
			if a.Account == captureAlias(uid) {
				a.Proxy = captureAlias(proxy)
				a.ProxyGroup = captureAlias(group)
			}
		}
	}
}

func (h *Handler) requestCaptureAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	m := &h.requestCapture
	if r.Method == http.MethodDelete {
		m.clear()
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if r.Method == http.MethodPost {
		var input struct {
			Enabled *bool  `json:"enabled"`
			Codes   *[]int `json:"business_codes"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		var extra any
		if d.Decode(&input) != nil || d.Decode(&extra) != io.EOF || input.Enabled == nil && input.Codes == nil {
			writeJSON(w, 400, map[string]any{"error": "无效设置"})
			return
		}
		if input.Codes != nil {
			if len(*input.Codes) == 0 || len(*input.Codes) > 16 {
				writeJSON(w, 400, map[string]any{"error": "请选择 1–16 个业务错误码"})
				return
			}
			for _, code := range *input.Codes {
				if code < 1 || code > 99999999 {
					writeJSON(w, 400, map[string]any{"error": "无效业务错误码"})
					return
				}
			}
		}
		m.mu.Lock()
		m.initLocked()
		m.epoch++
		if input.Enabled != nil {
			m.enabled = *input.Enabled
		}
		if input.Codes != nil {
			m.codes = append([]int(nil), (*input.Codes)...)
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.initLocked()
	m.expireLocked(time.Now())
	if id := r.PathValue("id"); id != "" {
		var raw []byte
		for _, record := range m.records {
			if record.summary["id"] == id {
				raw = bytes.Clone(record.data)
				break
			}
		}
		m.mu.Unlock()
		if raw == nil {
			writeJSON(w, 404, map[string]any{"error": "记录不存在或已过期"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
		return
	}
	rows := make([]map[string]any, 0, len(m.records))
	for i := len(m.records) - 1; i >= 0; i-- {
		rows = append(rows, m.records[i].summary)
	}
	data := map[string]any{"enabled": m.enabled, "business_codes": append([]int(nil), m.codes...), "records": rows, "in_flight": m.inFlight, "skipped_busy": m.skipped, "evicted": m.evicted, "bytes": m.bytes, "retention_seconds": int(captureRetention.Seconds()), "max_records": captureRecordLimit, "max_bytes": captureStoreLimit, "max_body_bytes": captureBodyLimit, "memory_only": true}
	m.mu.Unlock()
	writeJSON(w, 200, data)
}
