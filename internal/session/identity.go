package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Identity is resolved once, before random prompt rewriting, then shared by
// routing, prompt selection, fair queues and subsequent affinity consumers.
// Raw client IDs are only exposed to the legacy account migration adapter;
// they must never enter logs or persistent snapshots.
type Identity struct {
	Key       string
	Source    string
	namespace string
	raw       string
}

func (i Identity) Empty() bool { return i.Key == "" }
func (i Identity) AccountKey(model string) string {
	if i.Empty() {
		return ""
	}
	return ScopedKey(i.namespace, model, i.raw)
}
func (i Identity) LegacyRaw() string { return i.raw } // account migration only; never log

type Resolver struct{ salt []byte }

func NewResolver(salt []byte) *Resolver {
	if len(salt) == 0 {
		salt = make([]byte, 32)
		_, _ = rand.Read(salt)
	}
	return &Resolver{salt: append([]byte(nil), salt...)}
}
func (r *Router) Resolver() *Resolver { return NewResolver(r.cfg.Salt) }
func (r *Resolver) hash(v any) string {
	b, _ := json.Marshal(v)
	m := hmac.New(sha256.New, r.salt)
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}
func ScopedKey(namespace, model, raw string) string {
	b, _ := json.Marshal([]string{namespace, model, raw})
	v := sha256.Sum256(b)
	return hex.EncodeToString(v[:])
}
func ExplicitIdentity(doc map[string]any, headers http.Header) (string, string) {
	if meta, ok := doc["metadata"].(map[string]any); ok {
		if v, ok := meta["conversation_id"].(string); ok && v != "" {
			return v, "metadata.conversation_id"
		}
	}
	for _, key := range []string{"conversation_id", "conversation", "prompt_cache_key"} {
		if v, ok := doc[key].(string); ok && v != "" {
			return v, key
		}
	}
	for _, key := range []string{"X-WorkBuddy-Session-ID", "X-Session-ID", "X-Conversation-ID"} {
		if v := headers.Get(key); v != "" {
			return v, key
		}
	}
	return "", "none"
}

// PrefixDocument bounds canonicalization to the opening conversation, without
// copying an entire growing transcript. Call the normal protocol normalizer on
// this copy, bypassing random replacements, before passing it to Resolve.
func PrefixDocument(doc map[string]any) (map[string]any, error) {
	prefix := map[string]any{}
	for k, v := range doc {
		prefix[k] = v
	}
	msgs, _ := doc["messages"].([]any)
	end := len(msgs)
	for i, raw := range msgs {
		if m, ok := raw.(map[string]any); ok && strings.EqualFold(fmt.Sprint(m["role"]), "user") {
			end = i + 1
			break
		}
	}
	prefix["messages"] = msgs[:end]
	b, err := json.Marshal(prefix)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(b, &out)
	return out, err
}
func (r *Resolver) Resolve(namespace string, original, normalized map[string]any, headers http.Header) Identity {
	raw, source := ExplicitIdentity(original, headers)
	if raw == "" {
		messages, _ := normalized["messages"].([]any)
		first := -1
		for i, v := range messages {
			if m, ok := v.(map[string]any); ok && m["role"] == "user" {
				first = i
				break
			}
		}
		if first < 0 {
			return Identity{Source: "none"}
		}
		settings := map[string]any{}
		for k, v := range normalized {
			switch k {
			case "messages", "model", "stream", "metadata", "user", "prompt_cache_key", "prompt_cache_retention", "conversation", "conversation_id", "max_tokens", "max_completion_tokens", "temperature", "top_p", "seed", "stream_options", "n":
				continue
			}
			settings[k] = v
		}
		raw = "prefix:" + r.hash([]any{namespace, settings, messages[:first+1]})
		source = "prefix"
	}
	return Identity{Key: r.hash([]string{namespace, raw}), Source: source, namespace: namespace, raw: raw}
}

// LegacyPromptKey is read-only compatibility for the first deployed variant
// release. No new binding is created using this old first-user-only identity.
func LegacyPromptKey(seed []byte, namespace string, doc map[string]any, headers http.Header) string {
	raw, _ := ExplicitIdentity(doc, headers)
	fallback := false
	if raw == "" {
		messages, _ := doc["messages"].([]any)
		for _, v := range messages {
			m, _ := v.(map[string]any)
			if m["role"] != "user" {
				continue
			}
			b, err := json.Marshal(m["content"])
			if err == nil && string(b) != "null" && string(b) != `""` {
				raw = fmt.Sprintf("%x", sha256.Sum256(b))
				fallback = true
			}
			break
		}
	}
	if raw == "" {
		return ""
	}
	framed := fmt.Sprintf("%q/%t/%q", namespace, fallback, raw)
	m := hmac.New(sha256.New, seed)
	m.Write([]byte("prompt-session\x00" + framed))
	return hex.EncodeToString(m.Sum(nil))
}
