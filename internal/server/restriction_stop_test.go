package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestExplicitRestrictionStopsAccountRotationAndCaptures(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "aggregate", true: "stream"}[stream], func(t *testing.T) {
			calls := []string{}
			up := newFakeUpstream(t, func(token string) (int, string, bool) {
				calls = append(calls, token)
				return 403, `{"code":11140,"msg":"request illegal","requestId":"restricted-fixture","displayMsg":{"en":"Your account has been restricted due to unusual activity"}}`, false
			})
			p := testPoolWith(
				&auth.Auth{UID: "restricted", AccessToken: "restricted-token", ExpiresAt: 9999999999},
				&auth.Auth{UID: "healthy", AccessToken: "healthy-token", ExpiresAt: 9999999999},
			)
			p.SetCredits("restricted", 2000)
			h := newTestHandler(t, Config{Pool: p, Upstream: up, MaxRotate: 3})
			t.Cleanup(h.requestCapture.clear)
			body := `{"model":"glm-5.2","messages":[{"role":"user","content":"fixture"}],"stream":false}`
			if stream {
				body = strings.Replace(body, `"stream":false`, `"stream":true`, 1)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "upstream_account_restricted") {
				t.Fatalf("restriction response: status=%d body=%s", w.Code, w.Body.String())
			}
			if len(calls) != 1 || calls[0] != "Bearer restricted-token" {
				t.Fatalf("restricted payload replayed: calls=%v", calls)
			}
			restricted, _ := p.Status("restricted")
			healthy, _ := p.Status("healthy")
			if !restricted.Disabled || restricted.InFlight != 0 || healthy.Disabled || healthy.InFlight != 0 || healthy.ErrTotal != 0 {
				t.Fatalf("incorrect account state: restricted=%+v healthy=%+v", restricted, healthy)
			}
			var capture errorCapture
			if err := json.Unmarshal(captureStoredData(t, &h.requestCapture), &capture); err != nil {
				t.Fatal(err)
			}
			if capture.HTTP != 403 || len(capture.Attempts) != 1 || capture.Attempts[0].BusinessCode != 11140 {
				t.Fatalf("restriction evidence missing: %+v", capture)
			}
		})
	}
}
