package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryWebAuthority struct {
	mu   sync.Mutex
	raw  json.RawMessage
	fail bool
}

func (a *memoryWebAuthority) Update(_ context.Context, _ string, fn func(json.RawMessage) (json.RawMessage, error)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return errors.New("unavailable")
	}
	raw := a.raw
	if raw == nil {
		raw = json.RawMessage("null")
	}
	next, err := fn(append(json.RawMessage(nil), raw...))
	if err == nil {
		a.raw = append(json.RawMessage(nil), next...)
	}
	return err
}
func (a *memoryWebAuthority) View(_ context.Context, _ string) (json.RawMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return nil, errors.New("unavailable")
	}
	return append(json.RawMessage(nil), a.raw...), nil
}

func sharedWebHandler(a WebAuthority) *Handler {
	return &Handler{cfg: Config{APIKey: "shared-test-key", Authority: a}, sessions: map[string]time.Time{}, unlockAttempts: map[string]unlockAttempt{}}
}

func sharedWebUnlock(h *Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/admin/unlock", strings.NewReader(body))
	r.RemoteAddr = "192.0.2.8:1234"
	w := httptest.NewRecorder()
	h.unlock(w, r)
	return w
}

func TestSharedWebCookieSurvivesReleaseAndFailsClosed(t *testing.T) {
	a := &memoryWebAuthority{}
	old, next := sharedWebHandler(a), sharedWebHandler(a)
	if err := old.InitializeSharedWeb(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := next.InitializeSharedWeb(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	w := sharedWebUnlock(old, `{"key":"shared-test-key"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("unlock status %d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("session cookie missing")
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
	r.AddCookie(cookies[0])
	if !next.frontendSession(r) {
		t.Fatal("new release lost old cookie")
	}
	a.mu.Lock()
	if bytes.Contains(a.raw, []byte(cookies[0].Value)) || bytes.Contains(a.raw, []byte("shared-test-key")) {
		t.Fatal("plaintext credential persisted")
	}
	a.fail = true
	a.mu.Unlock()
	if old.frontendSession(r) || next.frontendSession(r) {
		t.Fatal("authority failure used stale session")
	}
	w = sharedWebUnlock(next, `{"key":"shared-test-key"}`)
	if w.Code != http.StatusServiceUnavailable || len(w.Result().Cookies()) != 0 {
		t.Fatal("issued session during authority outage")
	}
}

func TestSharedWebConcurrentUnlockRateLimitIsGlobal(t *testing.T) {
	a := &memoryWebAuthority{}
	old, next := sharedWebHandler(a), sharedWebHandler(a)
	if err := old.InitializeSharedWeb(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := next.InitializeSharedWeb(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	statuses := make(chan int, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := old
			if i%2 != 0 {
				h = next
			}
			statuses <- sharedWebUnlock(h, `{"key":"incorrect"}`).Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	denied, blocked := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusUnauthorized:
			denied++
		case http.StatusTooManyRequests:
			blocked++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if denied != unlockFailureLimit || blocked != 20-unlockFailureLimit {
		t.Fatalf("denied=%d blocked=%d", denied, blocked)
	}
	if w := sharedWebUnlock(next, `{"key":"shared-test-key"}`); w.Code != http.StatusTooManyRequests {
		t.Fatal("correct credential bypassed shared lockout")
	}
}

func TestSharedWebBootstrapImportsOnceAndRejectsLegacyOverwrite(t *testing.T) {
	a := &memoryWebAuthority{}
	candidate := sharedWebHandler(a)
	if err := candidate.InitializeSharedWeb(context.Background(), false); err == nil {
		t.Fatal("candidate created authority")
	}
	old := sharedWebHandler(a)
	old.cfg.Authority = nil
	old.sessions[sessionDigest("old-cookie")] = time.Now().Add(time.Hour)
	old.unlockAttempts["192.0.2.8"] = unlockAttempt{windowStart: time.Now(), failures: unlockFailureLimit, blockedUntil: time.Now().Add(time.Minute)}
	path := filepath.Join(t.TempDir(), "handoff.enc")
	if err := old.SaveHandoff(path, nil); err != nil {
		t.Fatal(err)
	}
	owner := sharedWebHandler(a)
	if _, err := owner.LoadHandoff(path); err != nil {
		t.Fatal(err)
	}
	if err := owner.InitializeSharedWeb(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := candidate.InitializeSharedWeb(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "wb2api_frontend", Value: "old-cookie"})
	if !candidate.frontendSession(r) {
		t.Fatal("bootstrap lost legacy cookie")
	}
	if w := sharedWebUnlock(candidate, `{"key":"shared-test-key"}`); w.Code != http.StatusTooManyRequests {
		t.Fatal("bootstrap lost legacy lockout")
	}
	if err := owner.SaveHandoff(path, nil); err == nil {
		t.Fatal("overlap saved legacy snapshot")
	}
	if _, err := owner.LoadHandoff(path); err == nil {
		t.Fatal("active authority accepted legacy import")
	}
	// A stale candidate cannot reinitialize existing authority with its map.
	candidate.sessions = map[string]time.Time{sessionDigest("stale-cookie"): time.Now().Add(time.Hour)}
	if err := candidate.InitializeSharedWeb(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	raw, _ := a.View(context.Background(), webAuthorityKey)
	state, err := decodeWebAuthority(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Sessions[sessionDigest("stale-cookie")]; ok {
		t.Fatal("candidate overwrote shared sessions")
	}
}
