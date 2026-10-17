package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// newBodyLimitEnv 组装带真实上游的 handler（413 判定发生在转发之前，
// 上游只会收到放行的请求）。ConfigPath 落在临时目录，验证落盘。
func newBodyLimitEnv(t *testing.T, cfg Config) (authedHandler, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := dir + "/config.json"
	seed := `{"schedule":{"checkin_hours":[9],"keepalive_hours":[22]}}`
	if err := os.WriteFile(configPath, []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = configPath
	if cfg.Pool == nil {
		cfg.Pool = testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	}
	if cfg.Upstream == nil {
		cfg.Upstream = newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	}
	return newTestHandler(t, cfg), configPath
}

// postChat 发一条 chat 请求，返回 (状态码, 响应体)。
func postChat(h authedHandler, body string) (int, string) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// chatBodyOf 生成一条总长恰为 size 字节的合法 chat 请求（padding 放在
// user message 里）。body 必须是合法 JSON 才能穿过 400 校验到达上游，
// 保证测试量的是 413 上限而不是 JSON 解析失败。骨架里 "PAD" 占 3 字节，
// 替换后总长 = size。
func chatBodyOf(size int) string {
	const skeleton = `{"model":"m","messages":[{"role":"user","content":"PAD"}]}`
	pad := size - len(skeleton) + len("PAD")
	if pad < 0 {
		pad = 0
	}
	return strings.Replace(skeleton, "PAD", strings.Repeat("a", pad), 1)
}

// TestMaxRequestBodyDefaultIs8MB：零值 Config（老部署/单测）默认 8 MB，
// 8 MB 内放行，超一字节 413。
func TestMaxRequestBodyDefaultIs8MB(t *testing.T) {
	h, _ := newBodyLimitEnv(t, Config{})

	// 恰好在上限内（默认 8 MB）：放行（fake 上游返回 200）。
	if code, body := postChat(h, chatBodyOf(defaultMaxRequestBodyBytes-1)); code != http.StatusOK {
		t.Fatalf("8MB-1 request = %d, want 200 (body=%s)", code, body)
	}
	// 超一字节：413，错误消息带当前上限。
	code, body := postChat(h, chatBodyOf(defaultMaxRequestBodyBytes+2))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("8MB+2 request = %d, want 413 (body=%s)", code, body)
	}
	if !strings.Contains(body, "exceeds 8 MB") {
		t.Errorf("413 message = %q, want current limit mentioned", body)
	}
}

// TestMaxRequestBodyConfigInjected：Config 注入的上限直接生效（config.json
// 的 max_request_body_mb 由 main 解析成字节传入）。
func TestMaxRequestBodyConfigInjected(t *testing.T) {
	h, _ := newBodyLimitEnv(t, Config{MaxRequestBodyBytes: 1_000_000}) // 1 MB

	if code, _ := postChat(h, chatBodyOf(1_000_000+1)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("1MB+1 request = %d, want 413", code)
	}
	if code, _ := postChat(h, chatBodyOf(1_000_000)); code != http.StatusOK {
		t.Fatalf("exactly 1MB request should pass")
	}
}

// TestAdminConfigMaxRequestBodyRoundTrip：WebUI 闭环——
// GET 回显当前值（老配置显示默认 8）；
// POST 保存后 (a) 运行时副本即时更新（下一条请求即按新上限判定），
// (b) config.json 落盘 max_request_body_mb（重启后仍生效）。
func TestAdminConfigMaxRequestBodyRoundTrip(t *testing.T) {
	h, configPath := newBodyLimitEnv(t, Config{})

	// GET：老配置没写该字段 → 回显默认 8 MB + 前端渲染所需的范围信息。
	out := doAdminConfig(t, h, http.MethodGet, nil)
	body, ok := out["max_request_body"].(map[string]any)
	if !ok {
		t.Fatalf("max_request_body missing in GET /admin/config: %v", out)
	}
	if body["mb"] != float64(8) {
		t.Errorf("mb = %v, want 8 (default)", body["mb"])
	}
	if body["min_mb"] != float64(1) || body["max_mb"] != float64(128) {
		t.Errorf("min/max = %v/%v, want 1/128", body["min_mb"], body["max_mb"])
	}

	// POST：改成 2 MB。
	out = doAdminConfig(t, h, http.MethodPost, map[string]any{
		"checkin_hours":    []int{9},
		"keepalive_hours":  []int{22},
		"max_request_body": map[string]any{"mb": 2},
	})
	updated, _ := out["updated"].(map[string]any)
	if updated["max_request_body_mb"] != float64(2) {
		t.Fatalf("updated.max_request_body_mb = %v, want 2", updated["max_request_body_mb"])
	}

	// 运行时即时生效：2 MB + 2 字节 → 413；消息里的上限也跟着变。
	code, msg := postChat(h, chatBodyOf(2_000_000+2))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("2MB+2 request = %d, want 413 (msg=%s)", code, msg)
	}
	if !strings.Contains(msg, "exceeds 2 MB") {
		t.Errorf("413 message = %q, want 2 MB limit mentioned", msg)
	}
	// 2 MB 内放行。
	if code, _ := postChat(h, chatBodyOf(2_000_000-1)); code != http.StatusOK {
		t.Fatalf("2MB-1 request should pass after WebUI save")
	}

	// 落盘：config.json 顶层出现 max_request_body_mb = 2。
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MaxRequestBodyMB int `json:"max_request_body_mb"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.MaxRequestBodyMB != 2 {
		t.Errorf("persisted max_request_body_mb = %d, want 2", doc.MaxRequestBodyMB)
	}

	// GET 回显已保存的值（不再是默认）。
	out = doAdminConfig(t, h, http.MethodGet, nil)
	if body, _ = out["max_request_body"].(map[string]any); body["mb"] != float64(2) {
		t.Errorf("GET after save: mb = %v, want 2", body["mb"])
	}
}

// TestAdminConfigMaxRequestBodyValidation：非法值（0/负数/超 128）返回 400，
// 且不落盘、运行时副本不变（413 行为仍按旧上限）。
func TestAdminConfigMaxRequestBodyValidation(t *testing.T) {
	h, configPath := newBodyLimitEnv(t, Config{MaxRequestBodyBytes: 2_000_000})
	before := h.currentMaxRequestBody()

	for _, mb := range []int{0, -1, 129, 1000} {
		req := httptest.NewRequest(http.MethodPost, "/admin/config", bytes.NewReader([]byte(
			fmt.Sprintf(`{"checkin_hours":[9],"keepalive_hours":[22],"max_request_body":{"mb":%d}}`, mb),
		)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("mb=%d accepted: %d %s", mb, rec.Code, rec.Body.String())
		}
	}

	if after := h.currentMaxRequestBody(); after != before {
		t.Errorf("runtime limit changed on rejected request: %d want %d", after, before)
	}
	raw, _ := os.ReadFile(configPath)
	if bytes.Contains(raw, []byte("max_request_body_mb")) {
		t.Error("rejected request must not persist the field")
	}
}

func TestAdminConfigMaxRequestBodyUpperBoundary(t *testing.T) {
	h, configPath := newBodyLimitEnv(t, Config{})
	doAdminConfig(t, h, http.MethodPost, map[string]any{
		"checkin_hours":    []int{9},
		"keepalive_hours":  []int{22},
		"max_request_body": map[string]any{"mb": 128},
	})
	if got := h.currentMaxRequestBody(); got != 128*1_000_000 {
		t.Fatalf("runtime limit = %d, want 128 MB", got)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MB int `json:"max_request_body_mb"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.MB != 128 {
		t.Fatalf("persisted limit = %d, want 128", doc.MB)
	}
	out := doAdminConfig(t, h, http.MethodGet, nil)
	body := out["max_request_body"].(map[string]any)
	if body["mb"] != float64(128) {
		t.Fatalf("GET limit = %v, want 128", body["mb"])
	}
}

// TestAdminConfigOmitMaxRequestBodyKeepsCurrent：POST 不带
// max_request_body（老前端只发 schedule）时，运行时上限与落盘都不动。
func TestAdminConfigOmitMaxRequestBodyKeepsCurrent(t *testing.T) {
	h, configPath := newBodyLimitEnv(t, Config{MaxRequestBodyBytes: 4_000_000})

	doAdminConfig(t, h, http.MethodPost, map[string]any{
		"checkin_hours":   []int{9},
		"keepalive_hours": []int{22},
	})

	if got := h.currentMaxRequestBody(); got != 4_000_000 {
		t.Errorf("omitted patch changed runtime limit: %d want %d", got, 4_000_000)
	}
	raw, _ := os.ReadFile(configPath)
	if bytes.Contains(raw, []byte("max_request_body_mb")) {
		t.Error("omitted patch must not write max_request_body_mb")
	}
}
