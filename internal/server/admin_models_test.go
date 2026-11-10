package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminModelsRequiresAdminCredential(t *testing.T) {
	h, groupKey, _ := newGroupTestHandler(t)
	h.cfg.CollectionRequired = true // Never contact a real supplier in this test.
	for _, tc := range []struct {
		name, key string
		want      int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"invalid", "wrong", http.StatusUnauthorized},
		{"group", groupKey, http.StatusUnauthorized},
		{"admin", "admin-key", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/models", nil)
			if tc.key != "" {
				req.Header.Set("Authorization", "Bearer "+tc.key)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d want %d", rec.Code, tc.want)
			}
			if rec.Code == http.StatusOK {
				var body struct {
					Object string `json:"object"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Object != "list" {
					t.Fatalf("not a model list: %s", rec.Body.String())
				}
			}
		})
	}
}

func TestAdminModelsAcceptsAPIKeyOnlyUnlockSession(t *testing.T) {
	h, _, _ := newGroupTestHandler(t)
	h.cfg.CollectionRequired = true
	if h.cfg.FrontendPassword != "" {
		t.Fatal("fixture must use API-key-only login")
	}
	unlock := httptest.NewRecorder()
	h.ServeHTTP(unlock, httptest.NewRequest(http.MethodPost, "/admin/unlock", strings.NewReader(`{"key":"admin-key"}`)))
	if unlock.Code != http.StatusOK {
		t.Fatalf("unlock: %d", unlock.Code)
	}
	cookies := unlock.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies=%d", len(cookies))
	}
	for _, path := range []string{"/admin/models", "/status", "/requests?limit=1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookies[0])
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("session on %s: %d", path, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/models", nil)
	req.AddCookie(&http.Cookie{Name: "wb2api_frontend", Value: "not-an-issued-session"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie accepted: %d", rec.Code)
	}
}
