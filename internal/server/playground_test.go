package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const playgroundChatBody = `{"request":{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]},"group":"vip","account_uid":"u1"}`

func playgroundTestRequest(h *Handler, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer admin-key")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPlaygroundAdminAuthenticationAndProxyOrigin(t *testing.T) {
	h, groupKey, _ := newGroupTestHandler(t)
	h.sessions[sessionDigest("issued-cookie")] = time.Now().Add(time.Hour)
	calls := 0
	h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 200, sseOK, true })
	for _, tc := range []struct {
		name, key, cookie, origin, site string
		want                            int
	}{
		{"admin-key", "admin-key", "", "", "", 200},
		{"https-proxy-cookie", "", "issued-cookie", "https://console.example", "same-origin", 200},
		{"cookie-without-origin", "", "issued-cookie", "", "", 403},
		{"foreign-origin", "", "issued-cookie", "https://evil.example", "same-origin", 403},
		{"cross-site", "", "issued-cookie", "https://console.example", "cross-site", 403},
		{"same-site", "", "issued-cookie", "https://console.example", "same-site", 403},
		{"group-key", groupKey, "", "", "", 401},
		{"cookie-plus-group-key", groupKey, "issued-cookie", "https://console.example", "same-origin", 401},
		{"anonymous", "", "", "", "", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://console.example/admin/playground/chat/completions", strings.NewReader(playgroundChatBody))
			r.Header.Set("Content-Type", "application/json")
			if tc.key != "" {
				r.Header.Set("Authorization", "Bearer "+tc.key)
			}
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: "wb2api_frontend", Value: tc.cookie})
			}
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r.Header.Set("X-Forwarded-Proto", "https") // Config.TrustedProxies remains empty.
			w := httptest.NewRecorder()
			before := calls
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != 200 && calls != before {
				t.Fatal("rejected request reached upstream")
			}
		})
	}
}

func TestPlaygroundOptionsIsReadOnlyAndSecretFree(t *testing.T) {
	h, groupKey, _ := newGroupTestHandler(t)
	h.cfg.Pool.Disable("u2", "private internal reason")
	a := h.cfg.Pool.AuthByUID("u1")
	before := a.Snapshot()
	h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) { t.Error("options called upstream"); return 500, "", false })
	r := httptest.NewRequest(http.MethodGet, "/admin/playground/options", nil)
	r.Header.Set("Authorization", "Bearer admin-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"snapshot_source":"local"`) {
		t.Fatal("snapshot must disclose its advisory local source")
	}
	var out struct {
		Groups   []playgroundGroup   `json:"groups"`
		Accounts []playgroundAccount `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 2 || len(out.Groups) != 2 {
		t.Fatalf("unexpected snapshot: %+v", out)
	}
	if out.Accounts[0].UID != "u1" || !out.Accounts[0].Available || out.Accounts[1].Available || out.Accounts[1].UnavailableReason != "disabled" {
		t.Fatalf("unexpected availability: %+v", out.Accounts)
	}
	if !reflect.DeepEqual(before, a.Snapshot()) {
		t.Fatal("snapshot changed account/device identity")
	}
	for _, secret := range []string{"admin-key", groupKey, "private internal reason", `"access_token"`, `"device_id"`, `"file_path"`} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("snapshot leaked %q", secret)
		}
	}
	r = httptest.NewRequest(http.MethodGet, "/admin/playground/options", nil)
	r.Header.Set("Authorization", "Bearer "+groupKey)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("group key options status=%d", w.Code)
	}
}

func TestPlaygroundSelectionAndStrictJSON(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
		code       string
	}{
		{"group-mismatch", strings.Replace(playgroundChatBody, `"u1"`, `"u2"`, 1), 409, "account_not_in_group"},
		{"unknown-group", strings.Replace(playgroundChatBody, `"vip"`, `"missing"`, 1), 400, "group_not_found"},
		{"unknown-account", strings.Replace(playgroundChatBody, `"u1"`, `"missing"`, 1), 404, "account_not_found"},
		{"unknown-envelope-field", `{"request":{"model":"glm-5.2"},"typo":1}`, 400, "invalid_request"},
		{"duplicate-envelope", `{"request":{"model":"glm-5.2"},"group":"vip","group":"default"}`, 400, "invalid_request"},
		{"duplicate-protocol", `{"request":{"model":"glm-5.2","model":"other"}}`, 400, "invalid_request"},
		{"trailing-json", playgroundChatBody + ` {}`, 400, "invalid_request"},
		{"null-request", `{"request":null}`, 400, "invalid_request"},
		{"invalid-stream", `{"request":{"model":"glm-5.2","stream":"true"}}`, 400, "invalid_request"},
		{"invalid-model", `{"request":{"model":42}}`, 400, "invalid_request"},
		{"invalid-history", `{"request":{"model":"glm-5.2","previous_response_id":null}}`, 400, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newGroupTestHandler(t)
			h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) { t.Error("invalid request reached upstream"); return 500, "", false })
			w := playgroundTestRequest(h, "/admin/playground/chat/completions", tc.body)
			if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	h, _, _ := newGroupTestHandler(t)
	h.cfg.Pool.Disable("u1", "disabled")
	w := playgroundTestRequest(h, "/admin/playground/chat/completions", playgroundChatBody)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "account_disabled") || !h.cfg.Pool.IsDisabled("u1") {
		t.Fatalf("disabled selection status=%d body=%s", w.Code, w.Body.String())
	}
	if err := h.cfg.Groups.Create("empty"); err != nil {
		t.Fatal(err)
	}
	w = playgroundTestRequest(h, "/admin/playground/chat/completions", `{"request":{"model":"glm-5.2"},"group":"empty"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "group_empty") {
		t.Fatalf("empty group status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPlaygroundPinnedAccountNeverFallsBack(t *testing.T) {
	h, _, _ := newGroupTestHandler(t)
	var calls []string
	h.cfg.Upstream = newFakeUpstream(t, func(token string) (int, string, bool) {
		calls = append(calls, token)
		if strings.Contains(token, "t1") {
			return 403, `{"code":11140,"msg":"request illegal t1 admin-key /app/auths/private.json https://user:password@proxy.example/private?token=hidden"}`, false
		}
		return 200, sseOK, true
	})
	w := playgroundTestRequest(h, "/admin/playground/chat/completions", playgroundChatBody)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "11140") || !strings.Contains(w.Body.String(), "request illegal") {
		t.Fatalf("lost upstream status or cause: %d %s", w.Code, w.Body.String())
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "t1") {
		t.Fatalf("pinned request rotated: %v", calls)
	}
	for _, secret := range []string{"t1", "admin-key", "private.json", "password", "proxy.example", "hidden"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("error leaked %q: %s", secret, w.Body.String())
		}
	}
	if w.Header().Get("X-Playground-Request-Id") == "" || !strings.Contains(w.Body.String(), w.Header().Get("X-Playground-Request-Id")) {
		t.Fatal("missing correlation ID")
	}
}

func TestPlaygroundAllowOnlyNarrowsWithMembershipChanges(t *testing.T) {
	h, _, _ := newGroupTestHandler(t)
	s := requestScope{kind: "playground", group: "vip", accountUID: "u1", selection: map[string]bool{"u1": true, "u2": true}}
	if got := h.allowSet(s); len(got) != 1 || !got["u1"] {
		t.Fatalf("selection=%v", got)
	}
	if err := h.cfg.Groups.SetAccountGroups("u1", []string{"default"}); err != nil {
		t.Fatal(err)
	}
	if got := h.allowSet(s); len(got) != 0 {
		t.Fatalf("removed membership still allowed: %v", got)
	}
}

func TestPlaygroundPinnedAccountHonorsModelCooldown(t *testing.T) {
	h, _, _ := newGroupTestHandler(t)
	h.cfg.Pool.CooldownModel("u1", "glm-5.2", time.Hour, "model rate limit")
	h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) {
		t.Error("model-restricted account reached upstream")
		return 200, sseOK, true
	})
	w := playgroundTestRequest(h, "/admin/playground/chat/completions", playgroundChatBody)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "account_unavailable") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPlaygroundBodyLimit(t *testing.T) {
	h, _, _ := newGroupTestHandler(t)
	h.maxRequestBody = 32
	w := playgroundTestRequest(h, "/admin/playground/chat/completions", playgroundChatBody)
	if w.Code != 413 || !strings.Contains(w.Body.String(), "request_too_large") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPlaygroundResponsesHistoryScopeIsolation(t *testing.T) {
	h, _, _ := newGroupTestHandler(t)
	calls := 0
	h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, strings.ReplaceAll(sseOK, "chatcmpl-1", fmt.Sprintf("chatcmpl-%d", calls)), true
	})
	first := playgroundTestRequest(h, "/admin/playground/responses", `{"request":{"model":"glm-5.2","input":"hello"},"group":"vip","account_uid":"u1"}`)
	if first.Code != 200 {
		t.Fatalf("initial status=%d body=%s", first.Code, first.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	id, _ := response["id"].(string)
	if id == "" {
		t.Fatal("missing response ID")
	}
	for _, tc := range []struct {
		group, uid string
		want       int
	}{{"vip", "u1", 200}, {"default", "u2", 400}, {"", "u1", 400}, {"vip", "", 400}} {
		body, _ := json.Marshal(map[string]any{"request": map[string]any{"model": "glm-5.2", "input": "continue", "previous_response_id": id}, "group": tc.group, "account_uid": tc.uid})
		before := calls
		w := playgroundTestRequest(h, "/admin/playground/responses", string(body))
		if w.Code != tc.want {
			t.Fatalf("scope %s/%s status=%d body=%s", tc.group, tc.uid, w.Code, w.Body.String())
		}
		if tc.want != 200 && (calls != before || !strings.Contains(w.Body.String(), "previous_response_scope_mismatch")) {
			t.Fatal("foreign history reached upstream or returned wrong error")
		}
	}
	public, _ := json.Marshal(map[string]any{"model": "glm-5.2", "input": "continue", "previous_response_id": id})
	before := calls
	w := playgroundTestRequest(h, "/v1/responses", string(public))
	if w.Code != 400 || calls != before || !strings.Contains(w.Body.String(), "previous_response_scope_mismatch") {
		t.Fatalf("public history escape: %d %s", w.Code, w.Body.String())
	}
}

func TestPlaygroundCancelledBeforeInference(t *testing.T) {
	h, _, _ := newGroupTestHandler(t)
	h.cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) { t.Error("cancelled request reached upstream"); return 500, "", false })
	r := httptest.NewRequest(http.MethodPost, "/admin/playground/chat/completions", strings.NewReader(playgroundChatBody))
	r.Header.Set("Authorization", "Bearer admin-key")
	r.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 408 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPlaygroundSSEErrorsRedactAcrossChunkBoundaries(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n"} {
		recorder := httptest.NewRecorder()
		recorder.Header().Set("Content-Type", "text/event-stream")
		w := &playgroundWriter{ResponseWriter: recorder, invocation: &playgroundInvocation{id: "pg-test", secrets: []string{"test-secret"}}}
		frame := "event: error" + ending + `data: {"error":{"message":"TLS handshake context deadline exceeded test-secret https://user:pass@proxy.test/a?token=x socks5://proxyuser:proxypassword@socks.test:1080 /app/auths/a.json"}}` + ending + ending
		for _, b := range []byte(frame) {
			if _, err := w.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
		}
		w.finish()
		got := recorder.Body.String()
		if !strings.Contains(got, "TLS handshake context deadline exceeded") || !strings.Contains(got, "pg-test") {
			t.Fatalf("lost error detail: %s", got)
		}
		for _, secret := range []string{"test-secret", "user:pass", "proxy.test", "proxypassword", "socks.test", "a.json"} {
			if strings.Contains(got, secret) {
				t.Fatalf("SSE leaked %q: %s", secret, got)
			}
		}
	}
	// A malformed provider error must not bypass redaction.
	r := httptest.NewRecorder()
	r.Header().Set("Content-Type", "text/event-stream")
	w := &playgroundWriter{ResponseWriter: r, invocation: &playgroundInvocation{id: "pg-test", secrets: []string{"test-secret"}}}
	_, _ = w.Write([]byte("event: error\ndata: test-secret TLS timeout\n\n"))
	w.finish()
	if strings.Contains(r.Body.String(), "test-secret") || !strings.Contains(r.Body.String(), "TLS timeout") {
		t.Fatalf("unsafe malformed error: %s", r.Body.String())
	}
}
