package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestModelsProtocolAndAuthentication(t *testing.T) {
	h := NewHandler(Config{APIKey: "models-secret", CollectionRequired: true})
	for _, tc := range []struct {
		name, key, bearer, version string
		status                     int
		anthropic                  bool
	}{
		{"openai", "", "models-secret", "", 200, false},
		{"anthropic_key", "models-secret", "", "", 200, true},
		{"anthropic_bearer", "", "models-secret", "2023-06-01", 200, true},
		{"invalid_key", "invalid", "", "", 401, true},
		{"invalid_bearer_has_priority", "models-secret", "invalid", "", 401, true},
		{"missing", "", "", "", 401, false},
		{"missing_anthropic", "", "", "2023-06-01", 401, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/models", nil)
			if tc.key != "" {
				req.Header.Set("x-api-key", tc.key)
			}
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			if tc.version != "" {
				req.Header.Set("anthropic-version", tc.version)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
			}
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if tc.status != 200 {
				if tc.anthropic && out["type"] != "error" {
					t.Fatalf("wrong error format: %v", out)
				}
				return
			}
			items := out["data"].([]any)
			if len(items) == 0 {
				t.Fatal("missing models")
			}
			first := items[0].(map[string]any)
			if tc.anthropic {
				if first["type"] != "model" || first["display_name"] == nil || first["created_at"] == nil || out["has_more"] == nil || out["first_id"] == nil || out["last_id"] == nil {
					t.Fatalf("invalid Anthropic models: %v", out)
				}
			} else if out["object"] != "list" || first["object"] != "model" || out["has_more"] != nil {
				t.Fatalf("OpenAI format changed: %v", out)
			}
			id := first["id"].(string)
			detailReq := httptest.NewRequest("GET", "/v1/models/"+id, nil)
			detailReq.Header = req.Header.Clone()
			detail := httptest.NewRecorder()
			h.ServeHTTP(detail, detailReq)
			if detail.Code != 200 {
				t.Fatal(detail.Body.String())
			}
			var item map[string]any
			_ = json.Unmarshal(detail.Body.Bytes(), &item)
			if !reflect.DeepEqual(item, first) {
				t.Fatalf("detail mismatch: %v != %v", item, first)
			}
			missing := httptest.NewRequest("GET", "/models/no-such-model", nil)
			missing.Header = req.Header.Clone()
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, missing)
			if rec.Code != 404 {
				t.Fatalf("missing model returned %d", rec.Code)
			}
		})
	}
}

func TestAnthropicModelsPagination(t *testing.T) {
	models := []map[string]any{{"id": "c", "created": 100}, {"id": "a", "created": 100}, {"id": "b", "created": 100}}
	for _, tc := range []struct {
		query string
		ids   []string
		more  bool
	}{
		{"", []string{"a", "b", "c"}, false},
		{"?limit=1", []string{"a"}, true},
		{"?limit=1&after_id=a", []string{"b"}, true},
		{"?limit=1&after_id=b", []string{"c"}, false},
		{"?after_id=c", []string{}, false},
		{"?limit=1&before_id=c", []string{"b"}, true},
		{"?limit=1&before_id=b", []string{"a"}, false},
		{"?before_id=a", []string{}, false},
	} {
		out, err := anthropicModelsPage(httptest.NewRequest("GET", "/v1/models"+tc.query, nil), models)
		if err != nil {
			t.Fatal(err)
		}
		items := out["data"].([]map[string]any)
		ids := []string{}
		for _, m := range items {
			ids = append(ids, m["id"].(string))
		}
		if !reflect.DeepEqual(ids, tc.ids) || out["has_more"] != tc.more {
			t.Fatalf("%s: %v", tc.query, out)
		}
		if len(items) == 0 && (out["first_id"] != nil || out["last_id"] != nil) {
			t.Fatal("empty page cursors must be null")
		}
	}
	if models[0]["id"] != "c" {
		t.Fatal("pagination mutated shared list")
	}
	for _, query := range []string{"?limit=0", "?limit=1001", "?limit=x", "?limit=", "?limit=1&limit=2", "?after_id=a&before_id=c", "?after_id=absent", "?before_id=absent"} {
		if _, err := anthropicModelsPage(httptest.NewRequest("GET", "/v1/models"+query, nil), models); err == nil {
			t.Fatalf("accepted %s", query)
		}
	}
}

func TestAnthropicModelsGroupScope(t *testing.T) {
	h, key, _ := newGroupTestHandler(t)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("x-api-key", key)
	rec := httptest.NewRecorder()
	h.withModelsAuth(func(w http.ResponseWriter, r *http.Request) {
		scope := scopeFromRequest(r)
		if scope.kind != "group" || scope.group != "vip" {
			t.Errorf("wrong scope: %+v", scope)
		}
		allowed := h.allowSet(scope)
		if !allowed["u1"] || allowed["u2"] {
			t.Errorf("wrong accounts: %v", allowed)
		}
		w.WriteHeader(204)
	})(rec, req)
	if rec.Code != 204 {
		t.Fatal(rec.Code)
	}
}
