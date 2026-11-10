//go:build linux

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/coordination"
	"workbuddy2api/internal/lifecycle"
)

func TestOverlapManagementAdmissionAndSharedDirectoryIdentity(t *testing.T) {
	dsn := os.Getenv("WB2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	dataDir := filepath.Join(dir, "data")
	for _, p := range []string{authDir, dataDir} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{AuthDir: authDir, StateFile: filepath.Join(dataDir, "state.json")}
	lock, coord := filepath.Join(dir, "writer.lock"), filepath.Join(dir, "members")
	a, err := lifecycle.JoinOverlap(lock, coord)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := lifecycle.JoinOverlap(lock, coord)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	s, err := coordination.OpenPostgres(context.Background(), dsn, fmt.Sprintf("main-overlap-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := &overlapRuntime{member: a, store: s}
	old.owner.Store(true)
	if err := old.register(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	next := &overlapRuntime{member: b, store: s}
	changed := *cfg
	changed.FrontendDir = "release-b/frontend"
	changed.Listen = "127.0.0.1:9999"
	if err := next.register(context.Background(), &changed); err != nil {
		t.Fatal("release address and assets must be independent", err)
	}
	called := 0
	h := old.manage(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++; w.WriteHeader(204) }))
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"POST", "/admin/config", 503}, {"GET", "/admin/config", 204}, {"POST", "/v1/chat/completions", 204}, {"POST", "/admin/unlock", 204}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s %s = %d", tc.method, tc.path, w.Code)
		}
	}
	if called != 3 {
		t.Fatal("management mutation escaped overlap freeze")
	}
	other := next.manage(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("standby received management") }))
	w := httptest.NewRecorder()
	other.ServeHTTP(w, httptest.NewRequest("GET", "/admin/config", nil))
	if w.Code != 503 {
		t.Fatal("standby management admitted")
	}
	// The same path strings alone cannot prove a shared container mount.
	before, err := overlapFingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(authDir, authDir+".old"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	after, err := overlapFingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("different credential directory accepted at identical path")
	}
}
