package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func awaitAuthRecovery(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("auth recovery did not finish")
		case <-tick.C:
		}
	}
}

func TestUnauthorizedRotatesAndDisablesFromRefresh(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(route+"/"+map[bool]string{true: "stream", false: "aggregate"}[stream], func(t *testing.T) {
				var chats, refreshes atomic.Int32
				a := &auth.Auth{UID: "bad", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: 9999999999}
				up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
				up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					status, body := 200, sseOK
					if strings.HasSuffix(r.URL.Path, "/token/refresh") {
						refreshes.Add(1)
						if r.Header.Get("X-Refresh-Token") != "old-refresh" {
							t.Error("refresh did not use the account credential")
						}
						if _, ok := r.Context().Deadline(); !ok {
							t.Error("recovery has no timeout")
						}
						status, body = 401, `{"code":12153,"msg":"12153:refresh token failed:400 Bad Request: invalid_grant: User disabled"}`
					} else if r.Header.Get("Authorization") == "Bearer old-access" {
						chats.Add(1)
						status, body = 401, `<html><h1>401 Authorization Required</h1><hr>openresty</html>`
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})
				p := testPoolWith(a, &auth.Auth{UID: "good", AccessToken: "good-access", ExpiresAt: 9999999999})
				p.SetCredits(a.UID, 2000)
				h := newTestHandler(t, Config{Pool: p, Upstream: up})
				body, _ := json.Marshal(map[string]any{"model": "glm-5.2", "messages": []map[string]string{{"role": "user", "content": "hi"}}, "input": "hi", "stream": stream})
				for range 3 {
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest("POST", route, strings.NewReader(string(body))))
					if rec.Code != 200 {
						t.Fatalf("request was not rescued: status=%d body=%s", rec.Code, rec.Body)
					}
				}
				awaitAuthRecovery(t, func() bool { st, _ := p.Status(a.UID); return st.Disabled })
				if chats.Load() != 1 || refreshes.Load() != 1 {
					t.Fatalf("bad chats=%d refreshes=%d, want one of each", chats.Load(), refreshes.Load())
				}
				st, _ := p.Status(a.UID)
				if st.Reason != "upstream account disabled (token refresh)" || st.InFlight != 0 {
					t.Fatalf("bad final account state: %+v", st)
				}
			})
		}
	}
}

func TestAuthRecoverySuccessPersistsAndRetainsCooldown(t *testing.T) {
	a := &auth.Auth{UID: "u", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: 9999999999, FilePath: filepath.Join(t.TempDir(), "auth.json")}
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}}`, false
	})
	p := testPoolWith(a)
	h := newTestHandler(t, Config{Pool: p, Upstream: up})
	h.recoverUnauthorized(a)
	awaitAuthRecovery(t, func() bool {
		raw, err := os.ReadFile(a.FilePath)
		return err == nil && strings.Contains(string(raw), "new-refresh")
	})
	st, _ := p.Status(a.UID)
	if st.Disabled || !st.Cooling || st.CoolKind != "auth" || a.Snapshot().AccessToken != "new-access" {
		t.Fatalf("successful refresh state: %+v", st)
	}
	if p.Pick() != nil {
		t.Fatal("late 401 can bypass recovery retry window")
	}
}

func TestAuthRecoveryConcurrentFailuresAreCoalesced(t *testing.T) {
	for _, response := range []string{"temporary", "missing-refresh"} {
		t.Run(response, func(t *testing.T) {
			var calls atomic.Int32
			a := &auth.Auth{UID: "u", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: 9999999999}
			if response == "missing-refresh" {
				a.RefreshToken = ""
			}
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				calls.Add(1)
				return 503, `{"msg":"temporarily unavailable"}`, false
			})
			p := testPoolWith(a)
			h := newTestHandler(t, Config{Pool: p, Upstream: up})
			var wg sync.WaitGroup
			for range 32 {
				wg.Go(func() { h.recoverUnauthorized(a) })
			}
			wg.Wait()
			st, _ := p.Status(a.UID)
			if st.Disabled || !st.Cooling || st.ErrTotal != 1 {
				t.Fatalf("transient failure must stay quarantined without permanent disable: %+v", st)
			}
			if response == "temporary" {
				awaitAuthRecovery(t, func() bool { return calls.Load() == 1 })
			}
			if calls.Load() > 1 || (response == "missing-refresh" && calls.Load() != 0) {
				t.Fatalf("refresh calls=%d", calls.Load())
			}
		})
	}
}

func TestExplicitAccountRestrictionDisablesWithoutRefresh(t *testing.T) {
	var calls atomic.Int32
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls.Add(1)
		return 403, `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity"}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u", AccessToken: "access", ExpiresAt: 9999999999})
	h := newTestHandler(t, Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[]}`)))
	st, _ := p.Status("u")
	if !st.Disabled || calls.Load() != 1 {
		t.Fatalf("explicit restriction not disabled immediately: calls=%d status=%+v", calls.Load(), st)
	}
}

func TestUnauthorizedDoesNotWaitForRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	a := &auth.Auth{UID: "bad", AccessToken: "bad", RefreshToken: "refresh", ExpiresAt: 9999999999}
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := 200, sseOK
		if strings.HasSuffix(r.URL.Path, "/token/refresh") {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			status, body = 503, `{"msg":"temporary"}`
		} else if r.Header.Get("Authorization") == "Bearer bad" {
			status, body = 401, `<html>401 Authorization Required</html>`
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	p := testPoolWith(a, &auth.Auth{UID: "good", AccessToken: "good", ExpiresAt: 9999999999})
	p.SetCredits(a.UID, 2000)
	h := newTestHandler(t, Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[]}`)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("client request waited for background refresh")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("background recovery did not start")
	}
	if rec.Code != 200 {
		t.Fatalf("healthy account did not rescue request: %d", rec.Code)
	}
}

func TestExpiredTokenUnauthorizedRefreshDoesNotLoop(t *testing.T) {
	var refreshes atomic.Int32
	a := &auth.Auth{UID: "bad", AccessToken: "bad", RefreshToken: "refresh", ExpiresAt: 1}
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := 200, sseOK
		if strings.HasSuffix(r.URL.Path, "/token/refresh") {
			refreshes.Add(1)
			status, body = 401, `<html>401 Authorization Required</html>`
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	p := testPoolWith(a, &auth.Auth{UID: "good", AccessToken: "good", ExpiresAt: 9999999999})
	p.SetCredits(a.UID, 2000)
	h := newTestHandler(t, Config{Pool: p, Upstream: up})
	for range 3 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[]}`)))
		if rec.Code != 200 {
			t.Fatalf("request failed: status=%d body=%s", rec.Code, rec.Body)
		}
	}
	st, _ := p.Status(a.UID)
	if st.Disabled || !st.Cooling || st.CoolKind != "auth" || refreshes.Load() != 1 {
		t.Fatalf("ambiguous refresh failure was not isolated: calls=%d status=%+v", refreshes.Load(), st)
	}
}
