package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/promptguard"
)

func TestImmediateWeightRefreshAPI(t *testing.T) {
	m := sharedPromptManager(t)
	c := m.Config()
	c.Rules[0].WeightRefreshHours = 6
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{APIKey: "admin", PromptGuard: m})
	body := map[string]any{"revision": m.Config().Revision, "rule_id": "r"}
	request := func(method, key, raw string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/admin/prompt-guard/weights/refresh", strings.NewReader(raw))
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	b, _ := json.Marshal(body)
	if w := request("POST", "", string(b)); w.Code != 401 {
		t.Fatal("public refresh allowed", w.Code)
	}
	w := request("POST", "admin", string(b))
	var updated promptguard.Config
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || updated.Rules[0].LastWeightRefresh == nil || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "nonce") || strings.Contains(w.Body.String(), "selection_seed") {
		t.Fatal("internal randomness exposed")
	}
	if w = request("POST", "admin", string(b)); w.Code != 409 {
		t.Fatal("stale refresh accepted", w.Code)
	}
	if w = request("GET", "admin", string(b)); w.Code == 200 {
		t.Fatal("GET changed weights")
	}
	if w = request("POST", "admin", `{"rule_id":"r","unexpected":true}`); w.Code != 400 {
		t.Fatal("unknown request fields accepted", w.Code)
	}
}
