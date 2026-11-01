package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func captureTestCall(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func captureTestRecords(t *testing.T, h http.Handler) []map[string]any {
	t.Helper()
	w := captureTestCall(t, h, "GET", "/admin/request-capture", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var doc struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Records
}
func captureTestHandler(t *testing.T, behavior func(string) (int, string, bool), count int) authedHandler {
	t.Helper()
	accounts := []*auth.Auth{}
	for i := 0; i < count; i++ {
		accounts = append(accounts, &auth.Auth{UID: fmt.Sprintf("private-uid-%d", i), AccessToken: fmt.Sprintf("private-upstream-token-%d", i), ExpiresAt: 9999999999})
	}
	h := newTestHandler(t, Config{Pool: testPoolWith(accounts...), Upstream: newFakeUpstream(t, behavior), AnthropicAPI: true, MaxRotate: count})
	t.Cleanup(h.requestCapture.clear)
	return h
}
func captureStoredData(t *testing.T, m *captureManager) []byte {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.records) == 0 {
		t.Fatal("expected stored capture")
	}
	return append([]byte(nil), m.records[0].data...)
}

func TestErrorCapturePreservesEarlierRestrictionAfterSuccessfulRetry(t *testing.T) {
	calls := 0
	h := captureTestHandler(t, func(string) (int, string, bool) {
		calls++
		if calls == 1 {
			return 403, `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity"}}`, false
		}
		return 200, routingSSE, true
	}, 2)
	input := `{"model":"deepseek-v4.1-flash","input":"hello","stream":true,"instructions":"fixture system instructions","tools":[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{}}}]}`
	w := captureTestCall(t, h, "POST", "/v1/responses", input)
	if w.Code != 200 || calls != 2 {
		t.Fatalf("status=%d calls=%d", w.Code, calls)
	}
	rows := captureTestRecords(t, h)
	if len(rows) != 1 {
		t.Fatalf("captures=%d", len(rows))
	}
	detail := captureTestCall(t, h, "GET", "/admin/request-capture/"+rows[0]["id"].(string), "")
	var got errorCapture
	if err := json.Unmarshal(detail.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.HTTP != 200 || len(got.Attempts) != 2 || got.Attempts[0].HTTP != 403 || got.Attempts[1].HTTP != 200 || got.Attempts[0].BusinessCode != 11140 {
		t.Fatalf("wrong attempt chain: %+v", got)
	}
	if !strings.Contains(string(got.Incoming.JSON), `"input"`) || !strings.Contains(string(got.Normalized.JSON), `"messages"`) || !strings.Contains(string(got.Attempts[0].Request.JSON), `"function"`) {
		t.Fatal("missing native/normalized/wire stages")
	}
	if got.RequestID == "" || got.Route != "/v1/responses" {
		t.Fatal("missing request correlation")
	}
	for _, s := range []string{"private-upstream-token-", "private-uid-", "Authorization"} {
		if strings.Contains(detail.Body.String(), s) {
			t.Fatalf("sensitive value leaked: %s", s)
		}
	}
}

func TestErrorCaptureSuccessIsDiscardedAndSSEErrorsAreCaptured(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"success", routingSSE, 0},
		{"sse", "data: {\"error\":{\"code\":11148,\"message\":\"bad tool message order\"}}\n\ndata: [DONE]\n\n", 1},
		{"sse_code", "data: {\"code\":11128,\"msg\":\"request illegal\"}\n\ndata: [DONE]\n\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := captureTestHandler(t, func(string) (int, string, bool) { return 200, tc.body, true }, 1)
			captureTestCall(t, h, "POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}],"stream":true}`)
			if rows := captureTestRecords(t, h); len(rows) != tc.want {
				t.Fatalf("captures=%d want=%d", len(rows), tc.want)
			}
		})
	}
}

func TestErrorCaptureMatchesConfiguredCodesOnlyAndDisableDiscardsInFlight(t *testing.T) {
	h := captureTestHandler(t, func(string) (int, string, bool) { return 400, `{"code":11148,"msg":"bad tool sequence"}`, false }, 1)
	w := captureTestCall(t, h, "POST", "/admin/request-capture", `{"business_codes":[11140]}`)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	captureTestCall(t, h, "POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`)
	if len(captureTestRecords(t, h)) != 0 {
		t.Fatal("nonselected code captured")
	}
	s := h.requestCapture.begin(httptest.NewRequest("POST", "/v1/responses", nil))
	if s == nil {
		t.Fatal("capture disabled unexpectedly")
	}
	s.match([]byte(`{"code":11140}`))
	captureTestCall(t, h, "POST", "/admin/request-capture", `{"enabled":false}`)
	s.finish()
	if len(captureTestRecords(t, h)) != 0 {
		t.Fatal("disabled capture stored an in-flight request")
	}
	if h.requestCapture.begin(httptest.NewRequest("POST", "/v1/responses", nil)) != nil {
		t.Fatal("disabled capture still allocates")
	}
}

func TestErrorCaptureRedactsHeadersArgumentsAndNestedErrorEchoes(t *testing.T) {
	m := &captureManager{}
	t.Cleanup(m.clear)
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer fixture-admin-secret")
	r.Header.Set("Cookie", "session=fixture-cookie-secret")
	r.Header.Set("Referer", "https://fixture.invalid/path?token=private-query")
	s := m.begin(r)
	s.record.Normalized = s.blob([]byte(`{"messages":[{"role":"user","content":"fixture-admin-secret Bearer fixture-cookie-secret sk-example-key-value"}],"tools":[{"type":"function","function":{"parameters":{"properties":{"password":{"type":"string"}}}}}],"arguments":"{\"password\":\"private-password\",\"api_key\":\"private-api-key\"}","url":"https://user:pass@fixture.invalid/path?api_key=private-query"}`), true)
	s.record.ClientErrors = append(s.record.ClientErrors, s.blob([]byte(`{"code":11140,"msg":"{\"access_token\":\"opaque-secret-value\"}"}`), true))
	s.match([]byte(`{"code":11140}`))
	s.finish()
	raw := string(captureStoredData(t, m))
	for _, value := range []string{"fixture-admin-secret", "fixture-cookie-secret", "private-password", "private-api-key", "private-query", "opaque-secret-value", "user:pass", "sk-example-key-value"} {
		if strings.Contains(raw, value) {
			t.Fatalf("secret leaked: %s", value)
		}
	}
	if !strings.Contains(raw, `"password":{"type":"string"}`) {
		t.Fatal("schema property was removed")
	}
}

func TestErrorCaptureRetentionEvictionAndClear(t *testing.T) {
	m := &captureManager{}
	t.Cleanup(m.clear)
	for i := 0; i < captureRecordLimit+3; i++ {
		s := m.begin(httptest.NewRequest("POST", "/v1/responses", nil))
		s.match([]byte(`{"code":11140}`))
		s.finish()
	}
	m.mu.Lock()
	count, evicted := len(m.records), m.evicted
	if count > 0 {
		m.records[0].expires = time.Now().Add(-time.Second)
	}
	m.expireLocked(time.Now())
	after := len(m.records)
	m.mu.Unlock()
	if count != captureRecordLimit || evicted != 3 {
		t.Fatal("record bound not enforced")
	}
	if after != captureRecordLimit-1 {
		t.Fatal("expiry not enforced")
	}
	s := m.begin(httptest.NewRequest("POST", "/v1/responses", nil))
	s.match([]byte(`{"code":11140}`))
	m.clear()
	s.finish()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.records) != 0 || m.bytes != 0 || m.inFlight != 0 {
		t.Fatal("clear resurrected in-flight content")
	}
}

func TestErrorCaptureSizeAndConcurrencyBounds(t *testing.T) {
	m := &captureManager{}
	t.Cleanup(m.clear)
	s := m.begin(httptest.NewRequest("POST", "/v1/responses", nil))
	s.record.Incoming = s.blob([]byte(`{"content":"`+strings.Repeat("x", captureBodyLimit)+`"}`), true)
	s.match([]byte(`{"code":11140}`))
	s.finish()
	var got errorCapture
	_ = json.Unmarshal(captureStoredData(t, m), &got)
	if !got.Incoming.Truncated || len(got.Incoming.JSON) != 0 {
		t.Fatal("oversized body was retained")
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	sessions := make(chan *captureSession, captureInFlightLimit*2)
	for i := 0; i < captureInFlightLimit*2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s := m.begin(httptest.NewRequest("POST", "/v1/responses", nil)); s != nil {
				sessions <- s
			}
		}()
	}
	close(start)
	wg.Wait()
	close(sessions)
	if len(sessions) != captureInFlightLimit {
		t.Fatal("in-flight cap not enforced")
	}
	for s := range sessions {
		s.finish()
	}
}

func TestErrorCaptureStoreByteLimit(t *testing.T) {
	m := &captureManager{}
	t.Cleanup(m.clear)
	m.records = []*storedCapture{{data: make([]byte, captureStoreLimit), expires: time.Now().Add(time.Hour)}}
	m.bytes = captureStoreLimit
	s := m.begin(httptest.NewRequest("POST", "/v1/responses", nil))
	s.match([]byte(`{"code":11140}`))
	s.finish()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bytes > captureStoreLimit || len(m.records) != 1 || m.evicted != 1 {
		t.Fatal("byte limit was not enforced")
	}
}

func TestErrorCaptureAcceptsShortConfiguredBusinessCode(t *testing.T) {
	m := &captureManager{}
	t.Cleanup(m.clear)
	s := m.begin(httptest.NewRequest("POST", "/v1/responses", nil))
	s.codes = map[int]bool{7: true}
	s.match([]byte(`{"code":7}`))
	s.finish()
	var got errorCapture
	_ = json.Unmarshal(captureStoredData(t, m), &got)
	if len(got.MatchedCodes) != 1 || got.MatchedCodes[0] != 7 {
		t.Fatal("configured numeric code did not match")
	}
}

func TestErrorCaptureAdminAuthorizationAndInvalidSettings(t *testing.T) {
	h, groupKey, _ := newGroupTestHandler(t)
	t.Cleanup(h.requestCapture.clear)
	for _, method := range []string{"GET", "POST", "DELETE"} {
		for _, key := range []string{"", groupKey} {
			r := httptest.NewRequest(method, "/admin/request-capture", strings.NewReader(`{"enabled":false}`))
			if key != "" {
				r.Header.Set("Authorization", "Bearer "+key)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 401 {
				t.Fatalf("%s group/anonymous status=%d", method, w.Code)
			}
		}
	}
	a := newTestHandler(t, Config{})
	t.Cleanup(a.requestCapture.clear)
	for _, body := range []string{`{}`, `{"business_codes":[]}`, `{"business_codes":[-1]}`, `{"enabled":true,"unknown":1}`, `{"enabled":true}{}`} {
		if w := captureTestCall(t, a, "POST", "/admin/request-capture", body); w.Code != 400 {
			t.Fatalf("invalid body accepted: %s", body)
		}
	}
	w := captureTestCall(t, a, "GET", "/admin/request-capture/expired", "")
	if w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("detail expiry/no-store failed")
	}
}

func TestErrorCaptureWriterPreservesFragmentedStreamAndFlush(t *testing.T) {
	dst := httptest.NewRecorder()
	w := &captureWriter{ResponseWriter: dst}
	parts := []string{"data: {\"er", "ror\":{\"code\":11140}}\n", "\ndata: [DONE]\n\n"}
	for _, part := range parts {
		n, e := w.Write([]byte(part))
		if n != len(part) || e != nil {
			t.Fatal(n, e)
		}
		w.Flush()
	}
	if dst.Body.String() != strings.Join(parts, "") || !dst.Flushed || len(w.errors) != 1 {
		t.Fatal("stream observation changed content or missed split error")
	}
	ctx := context.WithValue(context.Background(), captureContextKey{}, (*captureSession)(nil))
	captureNormalized(ctx, []byte(`{}`))
}

func TestErrorCaptureNativeMessagesAndEndpointAliases(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/messages", `{"model":"glm-5.2","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`},
		{"/chat/completions", `{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`},
		{"/responses", `{"model":"glm-5.2","input":"hello"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			h := captureTestHandler(t, func(string) (int, string, bool) { return 403, `{"code":11128,"msg":"blocked"}`, false }, 1)
			captureTestCall(t, h, "POST", tc.path, tc.body)
			rows := captureTestRecords(t, h)
			if len(rows) != 1 {
				t.Fatal("native/alias error not captured")
			}
			var got errorCapture
			_ = json.Unmarshal(captureStoredData(t, &h.requestCapture), &got)
			var original, captured any
			_ = json.Unmarshal([]byte(tc.body), &original)
			_ = json.Unmarshal(got.Incoming.JSON, &captured)
			hash := sha256.Sum256([]byte(tc.body))
			if got.Route != tc.path || !reflect.DeepEqual(original, captured) || got.Incoming.SHA256 != fmt.Sprintf("%x", hash) {
				t.Fatalf("native request was replaced by converted body: %s", got.Incoming.JSON)
			}
		})
	}
}

func TestErrorCaptureExpiresWithoutReadOrTraffic(t *testing.T) {
	m := &captureManager{}
	t.Cleanup(m.clear)
	s := m.begin(httptest.NewRequest("POST", "/v1/responses", nil))
	s.match([]byte(`{"code":11140}`))
	s.finish()
	m.mu.Lock()
	m.records[0].expires = time.Now().Add(10 * time.Millisecond)
	m.scheduleLocked()
	m.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		count := len(m.records)
		m.mu.Unlock()
		if count == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("expired capture retained without traffic")
}
