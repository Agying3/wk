package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoginRegionSelection(t *testing.T) {
	h := newTestHandler(t, Config{Region: "cn"})
	got, err := h.loginRegion(httptest.NewRequest("POST", "/admin/account/url?region=global", nil))
	if err != nil || got != "global" {
		t.Fatalf("global region=%q err=%v", got, err)
	}
	got, err = h.loginRegion(httptest.NewRequest("POST", "/admin/account/url?region=china", nil))
	if err != nil || got != "cn" {
		t.Fatalf("cn region=%q err=%v", got, err)
	}
	if _, err := h.loginRegion(httptest.NewRequest("POST", "/admin/account/url?region=all", nil)); err == nil {
		t.Fatal("all must not be accepted as a single login endpoint")
	}

	h.cfg.Region = "global"
	got, err = h.loginRegion(httptest.NewRequest("POST", "/admin/account/url", nil))
	if err != nil || got != "global" {
		t.Fatalf("configured global region=%q err=%v", got, err)
	}
}

func TestLoginCommandFailureClassification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses the Linux helper process contract")
	}
	for _, tc := range []struct {
		name, payload string
		status        int
		message       string
	}{
		{"waiting", `{"stage":"token","status":200,"code":11217}`, http.StatusConflict, "登录尚未完成"},
		{"seat_full", `{"stage":"authorization","status":403,"code":12005}`, http.StatusBadGateway, "席位已满"},
		{"transport", `{"stage":"token"}`, http.StatusBadGateway, "获取登录令牌失败"},
		{"unstructured", `secret-test-value`, http.StatusBadGateway, "登录请求失败"},
		{"unknown_stage", `{"stage":"secret-test-value"}`, http.StatusBadGateway, "登录请求失败"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", `printf '%s' "$LOGIN_TEST_ERROR" >&2; exit 1`)
			cmd.Env = setCommandEnv(os.Environ(), "LOGIN_TEST_ERROR", tc.payload)
			_, err := cmd.Output()
			if err == nil {
				t.Fatal("helper should fail")
			}
			status, message := loginCommandFailure(err)
			if status != tc.status || !strings.Contains(message, tc.message) || strings.Contains(message, "secret-test-value") {
				t.Fatalf("status=%d message=%q", status, message)
			}
		})
	}
}

func TestPromoteMixedRegionPersistsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"region":"cn","schedule":{"checkin_hours":[9]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, Config{ConfigPath: path, Region: "cn"})
	if err := h.promoteMixedRegionIfNeeded("global"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if h.cfg.Region != "all" {
		t.Fatalf("runtime region=%q want all", h.cfg.Region)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["region"] != "all" || doc["schedule"] == nil {
		t.Fatalf("config after promote=%v", doc)
	}
	// Repeated calls are idempotent and must not rewrite the configuration.
	if err := h.promoteMixedRegionIfNeeded("global"); err != nil {
		t.Fatalf("second promote: %v", err)
	}
}

func TestWriteFileAtomicFallsBackForMountedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	forcedRenameError := func(_, _ string) error { return errors.New("mounted file") }
	if err := writeFileAtomicWith(path, []byte("new"), 0o600, forcedRenameError); err != nil {
		t.Fatalf("fallback write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "new" {
		t.Fatalf("content=%q want new", raw)
	}
}
