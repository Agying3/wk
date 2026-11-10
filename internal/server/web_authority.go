package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// WebAuthority must provide an atomic read-modify-write operation. The shared
// coordination store scopes the fixed key below to one deployment domain.
type WebAuthority interface {
	Update(context.Context, string, func(json.RawMessage) (json.RawMessage, error)) error
	View(context.Context, string) (json.RawMessage, error)
}

const webAuthorityKey = "management/web-auth/v1"

type webAuthorityState struct {
	ready            atomic.Bool
	managementMu     sync.Mutex
	managementActive bool
}

type webAuthorityWire struct {
	Version  int                   `json:"version"`
	Sessions map[string]time.Time  `json:"sessions"`
	Attempts map[string]unlockWire `json:"attempts"`
	Failures int                   `json:"failures"`
	Window   time.Time             `json:"window"`
	Blocked  time.Time             `json:"blocked"`
}

func decodeWebAuthority(raw json.RawMessage) (webAuthorityWire, error) {
	var s webAuthorityWire
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if s.Version != 1 || s.Sessions == nil || s.Attempts == nil {
		return s, errors.New("shared web authority is absent or incompatible")
	}
	return s, nil
}

// InitializeSharedWeb imports the legacy handoff only into an empty authority,
// and only when bootstrap is true and the caller owns the management lease.
// Candidates must pass false. Call before exposing the HTTP listener.
func (h *Handler) InitializeSharedWeb(ctx context.Context, bootstrap bool) error {
	if h.cfg.Authority == nil {
		return errors.New("shared web authority not configured")
	}
	err := h.cfg.Authority.Update(ctx, webAuthorityKey, func(raw json.RawMessage) (json.RawMessage, error) {
		if strings.TrimSpace(string(raw)) != "null" {
			_, err := decodeWebAuthority(raw)
			return raw, err
		}
		if !bootstrap {
			return nil, errors.New("shared web authority requires owner initialization")
		}
		s := webAuthorityWire{Version: 1, Sessions: map[string]time.Time{}, Attempts: map[string]unlockWire{}}
		h.sessionsMu.Lock()
		for digest, expires := range h.sessions {
			if time.Now().Before(expires) {
				s.Sessions[digest] = expires
			}
		}
		h.sessionsMu.Unlock()
		h.unlockRateMu.Lock()
		for key, attempt := range h.unlockAttempts {
			s.Attempts[sessionDigest(key)] = unlockWire{attempt.windowStart, attempt.failures, attempt.blockedUntil}
		}
		s.Failures, s.Window, s.Blocked = h.unlockGlobalFailures, h.unlockGlobalWindowStart, h.unlockGlobalBlockedUntil
		h.unlockRateMu.Unlock()
		return json.Marshal(s)
	})
	if err == nil {
		h.webAuthority.ready.Store(true)
	}
	return err
}

func (h *Handler) sharedFrontendSession(ctx context.Context, token string) bool {
	if !h.webAuthority.ready.Load() {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := h.cfg.Authority.View(ctx, webAuthorityKey)
	if err != nil {
		return false
	}
	s, err := decodeWebAuthority(raw)
	if err != nil {
		return false
	}
	expires, ok := s.Sessions[sessionDigest(token)]
	return ok && time.Now().Before(expires)
}

func (s *webAuthorityWire) cleanup(now time.Time) {
	for digest, expires := range s.Sessions {
		if !now.Before(expires) {
			delete(s.Sessions, digest)
		}
	}
	for key, a := range s.Attempts {
		if now.Sub(a.WindowStart) >= unlockFailureWindow && !now.Before(a.BlockedUntil) {
			delete(s.Attempts, key)
		}
	}
}

func (s *webAuthorityWire) retryAfter(key string, now time.Time) int {
	until := s.Blocked
	if a := s.Attempts[key]; a.BlockedUntil.After(until) {
		until = a.BlockedUntil
	}
	if !now.Before(until) {
		return 0
	}
	seconds := int(until.Sub(now).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

func (s *webAuthorityWire) failure(key string, now time.Time) {
	a, exists := s.Attempts[key]
	if a.WindowStart.IsZero() || now.Sub(a.WindowStart) >= unlockFailureWindow {
		a = unlockWire{WindowStart: now}
	}
	a.Failures++
	if a.Failures >= unlockFailureLimit {
		a.BlockedUntil = now.Add(unlockBlockDuration)
	}
	// Bound attacker-controlled IP cardinality. The global counter is always
	// updated, including when the per-client map has reached its limit.
	if exists || len(s.Attempts) < 4096 {
		s.Attempts[key] = a
	}
	if s.Window.IsZero() || now.Sub(s.Window) >= unlockGlobalWindow {
		s.Window = now
		s.Failures = 0
	}
	s.Failures++
	if s.Failures >= unlockGlobalFailureLimit {
		s.Blocked = now.Add(unlockGlobalBlockDuration)
		s.Failures = 0
		s.Window = now
	}
}

// sharedUnlock performs rate admission, failure accounting and cookie issuance
// in the same transaction. Two releases cannot both pass a stale rate check.
func (h *Handler) sharedUnlock(w http.ResponseWriter, r *http.Request) {
	if !h.webAuthority.ready.Load() {
		sharedWebUnavailable(w)
		return
	}
	var req struct {
		Password string `json:"password"`
		Key      string `json:"key"`
	}
	validBody := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) == nil
	valid := validBody && ((req.Key != "" && h.cfg.APIKey != "" && req.Key == h.cfg.APIKey) ||
		(req.Password != "" && h.cfg.FrontendPassword != "" && req.Password == h.cfg.FrontendPassword))
	var token string
	if valid {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			sharedWebUnavailable(w)
			return
		}
		token = hex.EncodeToString(b)
	}
	key := sessionDigest(h.clientHost(r))
	retry := 0
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	err := h.cfg.Authority.Update(ctx, webAuthorityKey, func(raw json.RawMessage) (json.RawMessage, error) {
		s, err := decodeWebAuthority(raw)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		s.cleanup(now)
		retry = s.retryAfter(key, now)
		if retry == 0 {
			if !valid {
				s.failure(key, now)
			} else {
				if len(s.Sessions) >= 8192 {
					return nil, errors.New("shared web session capacity exceeded")
				}
				delete(s.Attempts, key)
				s.Sessions[sessionDigest(token)] = now.Add(24 * time.Hour)
			}
		}
		return json.Marshal(s)
	})
	if err != nil {
		sharedWebUnavailable(w)
		return
	}
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "尝试次数过多，请稍后重试"})
		return
	}
	if !valid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "密钥或密码错误"})
		return
	}
	secure := r.TLS != nil || (h.isTrustedProxy(remoteHost(r.RemoteAddr)) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https"))
	http.SetCookie(w, &http.Cookie{Name: "wb2api_frontend", Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func sharedWebUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "共享会话存储不可用，请稍后重试"})
}
