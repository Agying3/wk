package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSharedTraceSurvivesActualAccountRotation(t *testing.T) {
	p := routingPool()
	var headers []http.Header
	var accounts []string
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, routingSSE, true })
	up.SharedConversationTrace = true
	up.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		headers = append(headers, req.Header.Clone())
		accounts = append(accounts, req.Header.Get("Authorization"))
		body, _ := io.ReadAll(req.Body)
		if strings.Contains(string(body), "private-session") {
			t.Error("raw downstream session identity leaked into upstream body")
		}
		code, response := 200, routingSSE
		if len(headers) == 1 {
			code, response = 429, `{"code":6004}`
		}
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	h := newTestHandler(t, Config{Pool: p, Session: routingSession(p), Upstream: up, MaxRotate: 3})
	call := func() {
		request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","conversation_id":"private-session","messages":[{"role":"user","content":"hello"}]}`))
		request.Header.Set("X-Trace-ID", "private-downstream-trace")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	call()
	if len(headers) != 2 || accounts[0] == accounts[1] {
		t.Fatalf("rotation was changed: accounts=%v", accounts)
	}
	for _, key := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Trace-ID"} {
		if headers[0].Get(key) == "" || headers[0].Get(key) != headers[1].Get(key) {
			t.Fatalf("accounts did not share %s", key)
		}
	}
	if headers[0].Get("X-Trace-ID") == "private-downstream-trace" || headers[0].Get("X-Request-ID") == headers[1].Get("X-Request-ID") {
		t.Fatal("downstream trace leaked or attempt identity was reused")
	}
	call()
	if len(headers) != 3 || headers[2].Get("X-Conversation-ID") != headers[1].Get("X-Conversation-ID") || headers[2].Get("X-Trace-ID") == headers[1].Get("X-Trace-ID") {
		t.Fatal("follow-up did not preserve conversation and create a new trace")
	}
}
