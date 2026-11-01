package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/promptguard"
	"workbuddy2api/internal/upstream"
)

func TestPromptVariantsChatResponsesShareSessionAndIgnoreOtherRoles(t *testing.T) {
	m, err := promptguard.Open("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := m.Config()
	c.Rules = []promptguard.Rule{{ID: "r", Match: "original", Enabled: true, WeightRefreshHours: 6,
		Variants: []promptguard.Variant{{ID: "a", Replacement: "a", Weight: 1}, {ID: "b", Replacement: "b", Weight: 1}}}}
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 400, `{"code":11128}`, false })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "fake", ExpiresAt: 9999999999}), Upstream: up, PromptGuard: m, APIKey: "admin", MaxRotate: 1})
	for i := range 8 {
		path := "/v1/chat/completions"
		doc := map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "system", "content": "original"}, map[string]any{"role": "user", "content": "private user"}}, "metadata": map[string]any{"conversation_id": "stable"}}
		if i%2 == 1 {
			path = "/v1/responses"
			doc = map[string]any{"model": "glm-5.3-flash", "instructions": "original", "input": "private user", "prompt_cache_key": "stable"}
		}
		b, _ := json.Marshal(doc)
		r := httptest.NewRequest("POST", path, strings.NewReader(string(b)))
		r.Header.Set("Authorization", "Bearer admin")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	list := m.List()
	if len(list) != 1 || list[0].Occurrences != 8 {
		t.Fatalf("same session rewrote differently across protocols: %+v", list)
	}
	item, _ := m.Get(list[0].ID)
	if text := promptguard.Text(item.Outbound); text != "a" && text != "b" {
		t.Fatal("no candidate applied", text)
	}
}

func TestPromptSessionIdentityFallbackAndExplicitIsolation(t *testing.T) {
	h := newTestHandler(t, Config{Upstream: upstream.New(), APIKey: "test-secret"})
	identity := func(doc map[string]any, r *http.Request) string {
		resolved, err := h.resolveSession(doc, r)
		if err != nil {
			t.Fatal(err)
		}
		return resolved.Identity.Key
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	doc := func(system, first string) map[string]any {
		return map[string]any{"messages": []any{map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": first}}}
	}
	a := identity(doc("system-a", "hello"), r)
	if a == "" || a == identity(doc("system-b", "hello"), r) {
		t.Fatal("different system prompts merged by fallback identity")
	}
	if a == identity(doc("system-a", "different"), r) {
		t.Fatal("different first messages merged")
	}
	r.Header.Set("X-Session-ID", "explicit")
	b := identity(doc("dynamic", "hello"), r)
	if a == b || b != identity(doc("new", "different"), r) {
		t.Fatal("explicit session not preferred")
	}
	if strings.Contains(a, "hello") {
		t.Fatal("raw user message in session key")
	}
}
