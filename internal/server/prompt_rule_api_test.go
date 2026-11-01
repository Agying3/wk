package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/promptguard"
	"workbuddy2api/internal/upstream"
)

func TestRuleTestLocalAndUpstreamIsolation(t *testing.T) {
	m, err := promptguard.Open("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := m.Config()
	c.Rules = []promptguard.Rule{{Name: "fixture", Match: "original", Enabled: false, Variants: []promptguard.Variant{{ID: "chosen", Replacement: "rewritten", Weight: 1}, {ID: "other", Replacement: "do not select", Weight: 0}}}}
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	c = m.Config()
	calls := []string{}
	controlFails := false
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		calls = append(calls, body)
		if strings.Contains(body, "original") || controlFails {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"code":11128}`))
			return
		}
		_, _ = w.Write([]byte(sseOK))
	}))
	defer fake.Close()
	up := upstream.New()
	up.HTTP = fake.Client()
	up.ChatBaseCN = fake.URL
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "fixture", ExpiresAt: 9999999999}), Upstream: up, PromptGuard: m, APIKey: "admin"})
	payload := map[string]any{"revision": c.Revision, "rule_id": c.Rules[0].ID, "variant_id": "chosen", "text": "original original", "upstream": false}
	request := func(key string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(payload)
		r := httptest.NewRequest("POST", "/admin/prompt-guard/rule-test", strings.NewReader(string(body)))
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(""); w.Code != 401 {
		t.Fatalf("public test: %d", w.Code)
	}
	w := request("admin")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"output":"rewritten rewritten"`) || len(calls) != 0 {
		t.Fatalf("local: %d %s calls=%v", w.Code, w.Body, calls)
	}
	payload["upstream"], payload["model"], payload["account_scope"] = true, "glm-5.3-flash", map[string]any{"kind": "all"}
	w = request("admin")
	var result struct {
		Trace     []promptguard.Trace          `json:"trace"`
		Selection promptguard.AccountSelection `json:"selection"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(result.Trace) != 3 || result.Trace[0].Outcome != "pass" || result.Trace[1].Outcome != "blocked" || result.Trace[2].Outcome != "pass" || result.Selection.UID != "u1" {
		t.Fatalf("upstream: %d %s", w.Code, w.Body)
	}
	if len(calls) != 3 || !strings.Contains(calls[2], "rewritten rewritten") || strings.Contains(calls[2], "do not select") {
		t.Fatalf("wrong requests: %v", calls)
	}
	if len(m.List()) != 0 || m.Config().Revision != c.Revision || m.Config().Rules[0].Enabled {
		t.Fatal("test changed config or captured a synthetic case")
	}
	controlFails = true
	calls = nil
	w = request("admin")
	if w.Code != 200 || len(calls) != 1 {
		t.Fatalf("failed control did not stop: %d %s", w.Code, w.Body)
	}
	calls = nil
	payload["revision"] = c.Revision - 1
	if w := request("admin"); w.Code != http.StatusConflict || len(calls) != 0 {
		t.Fatalf("stale config tested: %d", w.Code)
	}
	payload["revision"], payload["text"] = c.Revision, "unmatched"
	if w := request("admin"); w.Code != 400 || len(calls) != 0 {
		t.Fatalf("unmatched test: %d", w.Code)
	}
}
