package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/promptguard"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

func sharedPromptManager(t *testing.T) *promptguard.Manager {
	t.Helper()
	m, err := promptguard.Open("", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	c := m.Config()
	c.Rules = []promptguard.Rule{{ID: "r", Match: "original", Enabled: true, Variants: []promptguard.Variant{{ID: "a", Replacement: "alpha", Weight: 1}, {ID: "b", Replacement: "beta", Weight: 1}}}}
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	return m
}
func sharedRequestDoc() map[string]any {
	return map[string]any{"model": "glm-5.2", "messages": []any{map[string]any{"role": "system", "content": "original"}, map[string]any{"role": "user", "content": "same opening"}}}
}

func TestSharedSessionResolvedOnceBeforePromptRewrite(t *testing.T) {
	m := sharedPromptManager(t)
	p := routingPool()
	accounts := routingSession(p)
	h := newTestHandler(t, Config{Pool: p, Session: accounts, PromptGuard: m, Upstream: upstream.New()})
	doc := sharedRequestDoc()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(context.WithValue(context.Background(), scopeKey{}, requestScope{kind: "admin"}))
	resolved, err := h.resolveSession(doc, req)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Identity.Source != "prefix" {
		t.Fatal("no shared inferred identity")
	}
	req = withRequestSession(req, resolved)
	if _, err = h.preparePromptBody(doc, req); err != nil {
		t.Fatal(err)
	}
	if got := promptguard.Text(promptguard.Extract(doc)); got == "original" {
		t.Fatal("fixture not rewritten")
	}
	again, err := h.resolveSession(doc, req)
	if err != nil || again.Identity.Key != resolved.Identity.Key {
		t.Fatal("consumer recomputed identity from rewritten prompt")
	}
	route := h.beginResolvedChatRoute(req, resolved.Identity, "glm-5.2", doc, nil)
	if route.key != resolved.Identity.AccountKey("glm-5.2") || route.lease.UID == "" {
		t.Fatal("account routing did not consume shared identity")
	}
	otherScope := req.WithContext(context.WithValue(req.Context(), scopeKey{}, requestScope{kind: "group", group: "other", keyID: "key"}))
	different, err := h.resolveSession(sharedRequestDoc(), otherScope)
	if err != nil || different.Identity.Key == resolved.Identity.Key {
		t.Fatal("context reuse crossed authorization namespace")
	}
}

func TestSharedSessionAccountMigrationKeepsPromptChoice(t *testing.T) {
	m := sharedPromptManager(t)
	p := routingPool()
	accounts := routingSession(p)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 400, `{"code":11128}`, false })
	h := newTestHandler(t, Config{Pool: p, Session: accounts, PromptGuard: m, Upstream: up, MaxRotate: 1})
	call := func() {
		b, _ := json.Marshal(sharedRequestDoc())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(b))))
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	call()
	items := m.List()
	if len(items) != 1 {
		t.Fatal(items)
	}
	first, _ := m.Get(items[0].ID)
	want := promptguard.Text(first.Outbound)
	p.CooldownModel(first.UID, "glm-5.2", time.Minute, "fixture")
	c := m.Config()
	for i := range c.Rules[0].Variants {
		if c.Rules[0].Variants[i].Replacement == want {
			c.Rules[0].Variants[i].Weight = 0
		}
	}
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	call()
	items = m.List()
	if len(items) != 1 || items[0].Occurrences != 2 {
		t.Fatal("same conversation changed prompt after account/weight change", items)
	}
	next, _ := m.Get(items[0].ID)
	if next.UID == first.UID || promptguard.Text(next.Outbound) != want {
		t.Fatal("migration changed prompt or retained cooled account")
	}
}

func TestSharedSessionCarriesOldInferredAccountBinding(t *testing.T) {
	p := routingPool()
	accounts := routingSession(p)
	up := upstream.New()
	h := newTestHandler(t, Config{Pool: p, Session: accounts, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(context.WithValue(context.Background(), scopeKey{}, requestScope{kind: "admin"}))
	doc := sharedRequestDoc()
	resolved, err := h.resolveSession(doc, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = up.PrepareChatBody(doc); err != nil {
		t.Fatal(err)
	}
	oldTrace := h.cacheRouter.Build(routeNamespace(req), "glm-5.2", doc)
	accounts.Bind(session.ScopedKey(routeNamespace(req), "glm-5.2", "prefix:"+oldTrace.Root), "A")
	route := h.beginResolvedChatRoute(req, resolved.Identity, "glm-5.2", doc, nil)
	if route.lease.UID != "A" {
		t.Fatal("upgrade discarded old prefix owner")
	}
	if lease, ok := accounts.LookupLease(resolved.Identity.AccountKey("glm-5.2")); !ok || lease.UID != "A" {
		t.Fatal("unified binding missing")
	}
}

func TestSharedSessionResponsesHonorsHeaderIdentity(t *testing.T) {
	m := sharedPromptManager(t)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 400, `{"code":11128}`, false })
	h := newTestHandler(t, Config{Pool: routingPool(), Upstream: up, PromptGuard: m, MaxRotate: 1})
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		doc := sharedRequestDoc()
		if strings.HasSuffix(path, "responses") {
			doc = map[string]any{"model": "glm-5.2", "instructions": "original", "input": "different current input"}
		}
		b, _ := json.Marshal(doc)
		req := httptest.NewRequest("POST", path, strings.NewReader(string(b)))
		req.Header.Set("X-Conversation-ID", "same-header-session")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	items := m.List()
	if len(items) != 1 || items[0].Occurrences != 2 {
		t.Fatal("Responses replaced explicit header with a random root", items)
	}
}

func TestSharedSessionResponsesHistoryKeepsPromptDuringAccountMigration(t *testing.T) {
	m := sharedPromptManager(t)
	p := routingPool()
	accounts := routingSession(p)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, routingSSE, true })
	transport := up.HTTP.Transport
	var prompts, auths []string
	up.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var doc map[string]any
		if err := json.NewDecoder(req.Body).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		prompts = append(prompts, promptguard.Text(promptguard.Extract(doc)))
		auths = append(auths, req.Header.Get("Authorization"))
		return transport.RoundTrip(req)
	})
	h := newTestHandler(t, Config{Pool: p, Session: accounts, Upstream: up, PromptGuard: m})
	first := doResponsesRequest(h, `{"model":"glm-5.2","instructions":"original","input":"opening"}`)
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	previous, _ := response["id"].(string)
	_, originalRoute, err := h.readResponseHistory(previous)
	if err != nil || previous == "" || originalRoute == "" {
		t.Fatal("history missing", err)
	}
	selected := strings.TrimPrefix(auths[0], "Bearer at-")
	p.CooldownModel(selected, "glm-5.2", time.Minute, "fixture")
	c := m.Config()
	for i := range c.Rules[0].Variants {
		if c.Rules[0].Variants[i].Replacement == prompts[0] {
			c.Rules[0].Variants[i].Weight = 0
		}
	}
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"model": "glm-5.2", "instructions": "original", "input": "continue", "previous_response_id": previous})
	second := doResponsesRequest(h, string(body))
	if second.Code != 200 {
		t.Fatal(second.Code, second.Body.String())
	}
	if len(prompts) != 2 || prompts[0] != prompts[1] || auths[0] == auths[1] {
		t.Fatal("history lost its shared identity during account migration", prompts, auths)
	}
}
