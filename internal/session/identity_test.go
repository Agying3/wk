package session

import (
	"net/http"
	"strings"
	"testing"
)

func identityDoc() map[string]any {
	return map[string]any{"model": "m", "messages": []any{map[string]any{"role": "system", "content": "system"}, map[string]any{"role": "user", "content": "task"}}, "tools": []any{map[string]any{"name": "tool-a"}}}
}

func TestUnifiedIdentityPrefixScopeAndAppend(t *testing.T) {
	r := NewResolver([]byte("fixed-identity-salt"))
	doc := identityDoc()
	resolve := func(namespace string, doc map[string]any) Identity { return r.Resolve(namespace, doc, doc, nil) }
	a := resolve("tenant-a", doc)
	doc["messages"] = append(doc["messages"].([]any), map[string]any{"role": "assistant", "content": "reply"}, map[string]any{"role": "user", "content": "continue"})
	doc["temperature"] = 0.8
	if a.Key != resolve("tenant-a", doc).Key {
		t.Fatal("history append or sampling setting changed session")
	}
	if a.Key == resolve("tenant-b", doc).Key {
		t.Fatal("tenant scopes collided")
	}
	doc["model"] = "other"
	if a.Key != resolve("tenant-a", doc).Key || a.AccountKey("m") == a.AccountKey("other") {
		t.Fatal("conversation and model-specific account scopes were conflated")
	}
	doc["tools"] = []any{map[string]any{"name": "tool-b"}}
	if a.Key == resolve("tenant-a", doc).Key {
		t.Fatal("different tool contexts merged")
	}
	doc = identityDoc()
	doc["messages"].([]any)[0].(map[string]any)["content"] = "other system"
	if a.Key == resolve("tenant-a", doc).Key {
		t.Fatal("different system contexts merged")
	}
	if strings.Contains(a.Key, "task") || len(a.Key) != 64 {
		t.Fatal("non-opaque session key")
	}
}

func TestUnifiedExplicitIdentityAndLegacyAccountKeys(t *testing.T) {
	r := NewResolver([]byte("fixed-salt"))
	doc := identityDoc()
	doc["metadata"] = map[string]any{"conversation_id": "conversation", "user_id": "shared-user"}
	a := r.Resolve("scope", doc, nil, nil)
	delete(doc, "metadata")
	doc["prompt_cache_key"] = "conversation"
	b := r.Resolve("scope", doc, nil, nil)
	delete(doc, "prompt_cache_key")
	c := r.Resolve("scope", doc, nil, http.Header{"X-Session-Id": []string{"conversation"}})
	if a.Key != b.Key || b.Key != c.Key {
		t.Fatal("ID field/header location split one conversation")
	}
	if a.AccountKey("m") != ScopedKey("scope", "m", "conversation") {
		t.Fatal("existing explicit account binding format changed")
	}
	doc["metadata"] = map[string]any{"user_id": "shared-user"}
	if id, _ := ExplicitIdentity(doc, nil); id != "" {
		t.Fatal("user_id used as conversation ID")
	}
	doc["messages"] = []any{map[string]any{"role": "system", "content": "only system"}}
	if !r.Resolve("scope", doc, doc, nil).Empty() {
		t.Fatal("invented anonymous identity without user context")
	}
	doc["conversation_id"] = "long-" + strings.Repeat("x", 1024) + "a"
	x := r.Resolve("scope", doc, nil, nil)
	doc["conversation_id"] = "long-" + strings.Repeat("x", 1024) + "b"
	if x.Key == r.Resolve("scope", doc, nil, nil).Key {
		t.Fatal("long session IDs truncated")
	}
}

func TestUnifiedPrefixCopyDoesNotMutateOriginal(t *testing.T) {
	doc := identityDoc()
	doc["messages"] = append(doc["messages"].([]any), map[string]any{"role": "assistant", "content": "large history"})
	prefix, err := PrefixDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefix["messages"].([]any)) != 2 {
		t.Fatal("copied full growing history")
	}
	prefix["messages"].([]any)[0].(map[string]any)["content"] = "rewritten"
	if doc["messages"].([]any)[0].(map[string]any)["content"] != "system" {
		t.Fatal("canonical prefix mutation affected outbound input")
	}
}
