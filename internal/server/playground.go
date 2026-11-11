package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

type playgroundRequest struct {
	Request    json.RawMessage `json:"request"`
	Group      string          `json:"group"`
	AccountUID string          `json:"account_uid"`
}

type playgroundAccount struct {
	UID               string   `json:"uid"`
	Nickname          string   `json:"nickname"`
	Region            string   `json:"region"`
	Groups            []string `json:"groups"`
	Disabled          bool     `json:"disabled"`
	Available         bool     `json:"available"`
	UnavailableReason string   `json:"unavailable_reason"`
}

type playgroundGroup struct {
	Name     string `json:"name"`
	Region   string `json:"region"`
	Accounts int    `json:"accounts"`
}

type playgroundContextKey struct{}
type playgroundInvocation struct {
	id       string
	secrets  []string
	accounts []*auth.Auth
}

func playgroundContext(r *http.Request) *playgroundInvocation {
	p, _ := r.Context().Value(playgroundContextKey{}).(*playgroundInvocation)
	return p
}

// Selection snapshots contain metadata only. Routing still acquires capacity
// through the existing pool and rechecks disabled/model cooldown state.
func (h *Handler) playgroundSnapshot(local bool) ([]playgroundGroup, []playgroundAccount, error) {
	if h.cfg.Pool == nil {
		return nil, nil, errors.New("account pool unavailable")
	}
	if !local && h.cfg.Groups != nil && h.cfg.Groups.CheckShared() != nil {
		return nil, nil, errors.New("group storage unavailable")
	}
	gs := []playgroundGroup{{Name: "default", Region: "all"}}
	var membershipsByUID map[string][]string
	if h.cfg.Groups != nil {
		gs = []playgroundGroup{}
		groupList, memberships := h.cfg.Groups.PlaygroundSnapshot()
		membershipsByUID = memberships
		if !local {
			groupList = h.cfg.Groups.ListDetailed()
		}
		for _, g := range groupList {
			gs = append(gs, playgroundGroup{Name: g.Name, Region: g.Region})
		}
	}
	states := map[string]pool.RoutingState{}
	statuses, routingStates := h.cfg.Pool.PlaygroundSnapshot()
	if !local {
		routingStates = h.cfg.Pool.RoutingStates("", nil)
		statuses = h.cfg.Pool.List()
	}
	for _, s := range routingStates {
		states[s.UID] = s
	}
	accounts := []playgroundAccount{}
	for _, a := range statuses {
		memberships := []string{"default"}
		if local && len(membershipsByUID[a.UID]) > 0 {
			memberships = membershipsByUID[a.UID]
		}
		if !local && h.cfg.Groups != nil {
			memberships = h.cfg.Groups.AccountGroups(a.UID)
		}
		s := states[a.UID]
		reason := ""
		switch {
		case a.Disabled:
			reason = "disabled"
		case !s.Healthy:
			reason = "cooling_or_unhealthy"
		case s.Limit > 0 && s.InFlight >= s.Limit:
			reason = "busy"
		case s.Waiting > 0:
			reason = "queued"
		}
		accounts = append(accounts, playgroundAccount{UID: a.UID, Nickname: a.Nickname, Region: a.Region, Groups: memberships, Disabled: a.Disabled, Available: reason == "", UnavailableReason: reason})
		for i := range gs {
			for _, name := range memberships {
				if gs[i].Name == name {
					gs[i].Accounts++
					break
				}
			}
		}
	}
	if !local && h.cfg.Pool.SharedError() != nil {
		return nil, nil, errors.New("account storage unavailable")
	}
	if !local && h.cfg.Groups != nil && h.cfg.Groups.CheckShared() != nil {
		return nil, nil, errors.New("group storage unavailable")
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].UID < accounts[j].UID })
	sort.Slice(gs, func(i, j int) bool { return gs[i].Name < gs[j].Name })
	return gs, accounts, nil
}

func (h *Handler) playgroundOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("Authorization") != "" && !h.validAPIKey(r) {
		writeOpenAIError(w, 401, "invalid_api_key", "admin credential required")
		return
	}
	gs, accounts, err := h.playgroundSnapshot(true)
	if err != nil {
		writeOpenAIError(w, 503, "playground_storage_unavailable", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"groups": gs, "accounts": accounts, "snapshot_source": "local"})
}

func (h *Handler) playgroundChat(w http.ResponseWriter, r *http.Request) {
	h.servePlayground(w, r, h.chatCompletions)
}
func (h *Handler) playgroundResponses(w http.ResponseWriter, r *http.Request) {
	h.servePlayground(w, r, h.responses)
}

func (h *Handler) servePlayground(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	inv := &playgroundInvocation{id: "pg_" + strings.TrimPrefix(newResponseID(), "resp_"), secrets: []string{h.cfg.APIKey, h.cfg.FrontendPassword}}
	for _, cookie := range r.Cookies() {
		inv.secrets = append(inv.secrets, cookie.Value)
	}
	r = r.WithContext(context.WithValue(r.Context(), playgroundContextKey{}, inv))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Playground-Request-Id", inv.id)
	w.Header().Set("X-Request-Id", inv.id)
	writer := &playgroundWriter{ResponseWriter: w, invocation: inv}
	defer writer.finish()
	if r.Header.Get("Authorization") != "" && !h.validAPIKey(r) {
		writeOpenAIError(writer, 401, "invalid_api_key", "admin credential required")
		return
	}
	if !h.playgroundSameOrigin(r) {
		writeOpenAIError(writer, 403, "playground_origin_rejected", "use the same-origin administration console for cookie-authenticated requests")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeOpenAIError(writer, 415, "invalid_content_type", "Content-Type must be application/json")
		return
	}
	// Preserve traffic attribution for explicit global-key requests; cookie
	// requests remain attributed to the console session.
	outerScope := scopeFromRequest(r)
	outerScope.adminKey = h.validAPIKey(r)
	r = r.WithContext(context.WithValue(r.Context(), scopeKey{}, outerScope))
	h.observeClientRequest(func(w http.ResponseWriter, r *http.Request) {
		raw, large, err := h.readRequestBody(r)
		if large {
			writeOpenAIError(w, 413, "request_too_large", "playground request exceeds the configured request body limit")
			return
		}
		if err != nil {
			writeOpenAIError(w, 400, "invalid_request", "unable to read request body")
			return
		}
		if !playgroundValidJSON(raw) {
			writeOpenAIError(w, 400, "invalid_request", "request must contain valid JSON without duplicate fields")
			return
		}
		var envelope playgroundRequest
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&envelope) != nil {
			writeOpenAIError(w, 400, "invalid_request", "expected request, group and account_uid fields")
			return
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(envelope.Request, &doc) != nil || doc == nil {
			writeOpenAIError(w, 400, "invalid_request", "request must be a JSON object")
			return
		}
		var model string
		if json.Unmarshal(doc["model"], &model) != nil || strings.TrimSpace(model) == "" {
			writeOpenAIError(w, 400, "invalid_request", "request.model must be a non-empty string")
			return
		}
		if value, ok := doc["stream"]; ok && string(value) != "true" && string(value) != "false" {
			writeOpenAIError(w, 400, "invalid_request", "request.stream must be a boolean")
			return
		}
		if value, ok := doc["previous_response_id"]; ok {
			var id string
			if json.Unmarshal(value, &id) != nil || string(value) == "null" {
				writeOpenAIError(w, 400, "invalid_request", "previous_response_id must be a string")
				return
			}
		}
		gs, accounts, err := h.playgroundSnapshot(false)
		if err != nil {
			writeOpenAIError(w, 503, "playground_storage_unavailable", err.Error())
			return
		}
		scope := requestScope{kind: "playground", group: envelope.Group, accountUID: envelope.AccountUID, selection: map[string]bool{}, adminKey: h.validAPIKey(r)}
		if scope.group != "" {
			found := false
			for _, g := range gs {
				found = found || g.Name == scope.group
			}
			if !found {
				writeOpenAIError(w, 400, "group_not_found", "selected group does not exist")
				return
			}
		}
		foundAccount := scope.accountUID == ""
		for _, a := range accounts {
			if scope.accountUID != "" && a.UID != scope.accountUID {
				continue
			}
			foundAccount = true
			inGroup := scope.group == ""
			for _, g := range a.Groups {
				inGroup = inGroup || g == scope.group
			}
			if !inGroup {
				continue
			}
			if scope.accountUID != "" && a.Disabled {
				writeOpenAIError(w, 409, "account_disabled", "selected account is disabled; no state was changed")
				return
			}
			scope.selection[a.UID] = true
			if a := h.cfg.Pool.AuthByUID(a.UID); a != nil {
				inv.accounts = append(inv.accounts, a)
			}
		}
		if !foundAccount {
			writeOpenAIError(w, 404, "account_not_found", "selected account does not exist")
			return
		}
		if len(scope.selection) == 0 {
			code, message := "group_empty", "selected group contains no accounts"
			if scope.accountUID != "" {
				code, message = "account_not_in_group", "selected account is not in the selected group"
			}
			writeOpenAIError(w, 409, code, message)
			return
		}
		if h.cfg.Pool.SharedError() != nil {
			writeOpenAIError(w, 503, "playground_storage_unavailable", "account storage unavailable")
			return
		}
		if scope.accountUID != "" && !h.cfg.Pool.HealthyForModel(scope.accountUID, model) {
			writeOpenAIError(w, 409, "account_unavailable", "selected account is cooling, restricted for this model, or otherwise unavailable")
			return
		}
		if r.Context().Err() != nil {
			writeOpenAIError(w, 408, "request_cancelled", "request cancelled before inference")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), scopeKey{}, scope))
		r.Body = io.NopCloser(bytes.NewReader(envelope.Request))
		r.ContentLength = int64(len(envelope.Request))
		next(w, r)
	}, writer, r)
}

func (h *Handler) playgroundSameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !strings.EqualFold(u.Host, r.Host) || (u.Scheme != "http" && u.Scheme != "https") {
			return false
		}
		// TLS may terminate before an untrusted loopback handoff proxy. Browser
		// fetch metadata proves same-origin without trusting forwarded headers.
		if r.Header.Get("Sec-Fetch-Site") == "same-origin" {
			return true
		}
		scheme := "http"
		if r.TLS != nil || h.isTrustedProxy(remoteHost(r.RemoteAddr)) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
			scheme = "https"
		}
		return u.Scheme == scheme
	}
	// Non-browser clients with explicit admin authentication do not need Origin.
	// Browsers using cookies must positively attest same-origin via fetch metadata.
	return h.validAPIKey(r) || r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

func (h *Handler) playgroundAllow(s requestScope) map[string]bool {
	out := map[string]bool{}
	if h.cfg.Pool == nil {
		return out
	}
	if h.cfg.Groups != nil && h.cfg.Groups.CheckShared() != nil {
		return out
	}
	for uid, yes := range s.selection {
		if !yes || s.accountUID != "" && uid != s.accountUID {
			continue
		}
		if s.group != "" {
			memberships := []string{"default"}
			if h.cfg.Groups != nil {
				memberships = h.cfg.Groups.AccountGroups(uid)
			}
			found := false
			for _, g := range memberships {
				found = found || g == s.group
			}
			if !found {
				continue
			}
		}
		out[uid] = true
	}
	if h.cfg.Groups != nil && h.cfg.Groups.CheckShared() != nil {
		return map[string]bool{}
	}
	return out
}

// Reserve a history namespace without changing public response IDs or the
// durable store format. Public API requests cannot load a playground transcript.
const playgroundHistoryPrefix = "\x00wb-playground-v1:"

func (h *Handler) playgroundHistoryScope(r *http.Request) string {
	// The resolver uses the existing private, restart-stable session salt.
	// A public caller cannot forge a valid scope tag via conversation_id.
	return h.sessionResolver.Resolve("playground-history-v1", map[string]any{"conversation_id": routeNamespace(r)}, nil, nil).Key
}
func (h *Handler) playgroundStoreRoute(r *http.Request, route string) string {
	if scopeFromRequest(r).kind != "playground" {
		return route
	}
	return playgroundHistoryPrefix + h.playgroundHistoryScope(r) + ":" + base64.RawURLEncoding.EncodeToString([]byte(route))
}
func (h *Handler) playgroundHistoryRoute(r *http.Request, stored string) (string, bool) {
	if !strings.HasPrefix(stored, playgroundHistoryPrefix) {
		return stored, scopeFromRequest(r).kind != "playground"
	}
	if scopeFromRequest(r).kind != "playground" {
		return "", false
	}
	part, encoded, ok := strings.Cut(strings.TrimPrefix(stored, playgroundHistoryPrefix), ":")
	if !ok || part != h.playgroundHistoryScope(r) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	return string(raw), err == nil && len(raw) > 0
}

func finishPlaygroundStat(r *http.Request, st *chatStat) {
	if p := playgroundContext(r); p != nil {
		st.id = p.id
		st.errorMessage = p.safeText(st.errorMessage)
	}
}
func finishPlaygroundError(w http.ResponseWriter, r *http.Request, st *chatStat, err error) bool {
	if playgroundContext(r) == nil {
		return false
	}
	status, code, message := 503, "no_healthy_account", "all selected accounts are unavailable (cooling, disabled, busy or model-restricted)"
	var ue *upstream.Error
	if errors.As(err, &ue) {
		if ue.Status >= 400 && ue.Status <= 599 {
			status = ue.Status
		}
		code = "upstream_" + ue.Kind.String()
		message = ue.Msg
	} else if err != nil {
		code = "transport_error"
		message = err.Error()
	}
	st.status = status
	setRequestError(st, code, message)
	writeOpenAIError(w, status, code, message)
	return true
}

// Reject ambiguous duplicate keys and deeply nested input before either the
// envelope or protocol decoder applies its own interpretation.
func playgroundValidJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !value(depth + 1) {
					return false
				}
			}
			token, err = d.Token()
			return err == nil && token == json.Delim('}')
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			token, err = d.Token()
			return err == nil && token == json.Delim(']')
		}
		return false
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

var playgroundPathPattern = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|/(?:app|opt|home|root|srv|tmp|run|etc|var)/)[^\s"'<>]*`)
var playgroundProxyURLPattern = regexp.MustCompile(`(?i)\b(?:https?|socks5h?|socks4a?)://[^\s<>"'\\]+`)

func (p *playgroundInvocation) safeText(text string) string {
	secrets := append([]string{}, p.secrets...)
	for _, a := range p.accounts {
		c := a.Snapshot()
		secrets = append(secrets, c.AccessToken, c.RefreshToken, c.FilePath)
	}
	// Exact credential matches are redacted even for short synthetic/test keys.
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	text = safeCaptureText(text, nil)
	text = playgroundProxyURLPattern.ReplaceAllString(text, "[URL]")
	return playgroundPathPattern.ReplaceAllString(text, "[PATH]")
}

// HTTP errors are buffered until redacted. SSE success frames remain streaming;
// only structured error/failed frames are rewritten. A bounded event buffer
// handles splits across Write calls without leaking a partial error frame.
type playgroundWriter struct {
	http.ResponseWriter
	invocation *playgroundInvocation
	status     int
	buffer     bytes.Buffer
	stream     bool
}

func (w *playgroundWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *playgroundWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	for key, values := range w.Header() {
		if strings.Contains(strings.ToLower(key), "request-id") {
			for i, value := range values {
				values[i] = w.invocation.safeText(value)
			}
			w.Header()[key] = values
		}
	}
	w.stream = strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") && status < 400
	if status < 400 {
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *playgroundWriter) Write(raw []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if w.status >= 400 {
		if w.buffer.Len() < 128<<10 {
			w.buffer.Write(raw[:min(len(raw), (128<<10)-w.buffer.Len())])
		}
		return len(raw), nil
	}
	if !w.stream {
		return w.ResponseWriter.Write(raw)
	}
	if w.buffer.Len()+len(raw) > 2<<20 {
		return 0, errors.New("playground SSE event exceeds safety limit")
	}
	w.buffer.Write(raw)
	for {
		data := w.buffer.Bytes()
		at := bytes.Index(data, []byte("\n\n"))
		end := at + 2
		if cr := bytes.Index(data, []byte("\r\n\r\n")); cr >= 0 && (at < 0 || cr < at) {
			at = cr
			end = cr + 4
		}
		if at < 0 {
			break
		}
		frame := append([]byte{}, w.buffer.Next(end)...)
		if _, err := w.ResponseWriter.Write(w.cleanEvent(frame)); err != nil {
			return 0, err
		}
	}
	return len(raw), nil
}
func (w *playgroundWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if w.status < 400 {
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}
func (w *playgroundWriter) cleanEvent(frame []byte) []byte {
	lines := bytes.Split(frame, []byte("\n"))
	failed := false
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("event:")) && (bytes.Contains(line, []byte("error")) || bytes.Contains(line, []byte("failed"))) {
			failed = true
		}
	}
	for i, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		var doc map[string]any
		if json.Unmarshal(payload, &doc) != nil || doc == nil {
			if failed {
				b, _ := json.Marshal(w.cleanJSON(map[string]any{"error": map[string]any{"code": "stream_error", "message": string(payload)}}))
				lines[i] = append([]byte("data: "), b...)
			}
			continue
		}
		typ, _ := doc["type"].(string)
		status, _ := doc["status"].(string)
		if failed || doc["error"] != nil || strings.Contains(typ, "error") || strings.Contains(typ, "failed") || status == "failed" {
			clean := w.cleanJSON(doc)
			b, _ := json.Marshal(clean)
			lines[i] = append([]byte("data: "), b...)
		}
	}
	return bytes.Join(lines, []byte("\n"))
}
func (w *playgroundWriter) cleanJSON(doc map[string]any) map[string]any {
	var clean func(any) any
	clean = func(v any) any {
		switch x := v.(type) {
		case string:
			return w.invocation.safeText(x)
		case map[string]any:
			for k, v := range x {
				x[k] = clean(v)
			}
		case []any:
			for i, v := range x {
				x[i] = clean(v)
			}
		}
		return v
	}
	doc = captureRedact(doc, nil, 0).(map[string]any)
	doc = clean(doc).(map[string]any)
	doc["playground_request_id"] = w.invocation.id
	if w.status >= 400 {
		doc["http_status"] = w.status
	}
	return doc
}
func (w *playgroundWriter) finish() {
	if w.status >= 400 {
		var doc map[string]any
		if json.Unmarshal(w.buffer.Bytes(), &doc) != nil || doc == nil {
			doc = map[string]any{"error": map[string]any{"code": "request_failed", "message": "request failed; consult the request log using playground_request_id"}}
		}
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Type", "application/json")
		w.ResponseWriter.WriteHeader(w.status)
		_ = json.NewEncoder(w.ResponseWriter).Encode(w.cleanJSON(doc))
	} else if w.stream && w.buffer.Len() > 0 {
		_, _ = w.ResponseWriter.Write(w.cleanEvent(w.buffer.Bytes()))
	}
}
