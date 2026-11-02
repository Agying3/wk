package upstream

import (
	"encoding/json"
	"fmt"
	"testing"
)

func prepareThinkingFixture(t *testing.T, input string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(mustPrepareBody(t, []byte(input), false), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDeepSeekThinkingControlAndHistory(t *testing.T) {
	d := prepareThinkingFixture(t, `{"model":"deepseek-v4.1-flash","reasoning_effort":"high","messages":[{"role":"assistant","reasoning_content":"original chain","content":"answer"},{"role":"assistant","reasoning_content":"","content":"another"},{"role":"user","content":"continue"}]}`)
	if d["thinking"].(map[string]any)["type"] != "enabled" || d["reasoning_effort"] != "high" {
		t.Fatal("thinking not enabled with original effort")
	}
	msgs := d["messages"].([]any)
	first, second := msgs[1].(map[string]any), msgs[2].(map[string]any)
	if first["reasoning_content"] != "original chain" || first["reasoning"] != "original chain" || second["reasoning"] != " " {
		t.Fatal("history reasoning lost")
	}
	for _, input := range []string{
		`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","reasoningEffort":"low","messages":[{"role":"assistant","content":"answer"}]}`,
		`{"model":"glm-5.3","messages":[{"role":"assistant","content":"answer"}]}`,
	} {
		out := prepareThinkingFixture(t, input)
		for _, raw := range out["messages"].([]any) {
			m := raw.(map[string]any)
			if m["role"] == "assistant" {
				if _, ok := m["reasoning"]; ok {
					t.Fatal("inactive/non-DeepSeek history changed")
				}
			}
		}
		if out["model"] == "glm-5.3" {
			if _, ok := out["thinking"]; ok {
				t.Fatal("non-DeepSeek thinking changed")
			}
		} else {
			if out["thinking"].(map[string]any)["type"] != "disabled" {
				t.Fatal("disabled overridden")
			}
			if _, ok := out["reasoning_effort"]; ok {
				t.Fatal("disabled kept effort")
			}
			if _, ok := out["reasoningEffort"]; ok {
				t.Fatal("disabled kept camel effort")
			}
		}
	}
}

func TestDeepSeekThinkingReasoningTypesAndDefaults(t *testing.T) {
	d := prepareThinkingFixture(t, `{"model":"deepseek-r1","messages":[{"role":"assistant","content":"a","reasoning_content":null,"reasoning":"keep"},{"role":"assistant","content":"b","reasoning_content":12},{"role":"assistant","content":"c","reasoning_content":"rc","reasoning":"preserve"}]}`)
	if d["reasoning_effort"] != "high" {
		t.Fatal("default effort missing")
	}
	m := d["messages"].([]any)
	if m[1].(map[string]any)["reasoning_content"] != "keep" || m[2].(map[string]any)["reasoning"] != " " || m[3].(map[string]any)["reasoning"] != "preserve" {
		t.Fatal("reasoning types/defaults wrong")
	}
	disabled := prepareThinkingFixture(t, `{"model":"deepseek-r1","thinking":{"type":"disabled"},"messages":[{"role":"assistant","content":"a","reasoning_content":"old chain"},{"role":"assistant","content":"b"}]}`)
	if disabled["messages"].([]any)[2].(map[string]any)["reasoning"] != " " {
		t.Fatal("disabled traced history not backfilled")
	}
	body := mustPrepareBodyWithEfforts(t, []byte(`{"model":"deepseek-r1","messages":[]}`), false, map[string][]string{"deepseek-r1": {"low"}})
	var limited map[string]any
	_ = json.Unmarshal(body, &limited)
	if limited["reasoning_effort"] != "low" {
		t.Fatal("effort capability normalization bypassed")
	}
}

func TestDeepSeekCapturedShapeHistoryPreservesToolPairing(t *testing.T) {
	// Structure-only fixture: no captured prompt, arguments or tool results.
	msgs := []any{map[string]any{"role": "system", "content": "synthetic fixture"}}
	for i := 0; i < 737; i++ {
		if i < 431 {
			msgs = append(msgs, map[string]any{"role": "user", "content": "fixture"})
		}
		count := 1
		if i < 27 {
			count = 2
		}
		calls := []any{}
		for j := 0; j < count; j++ {
			id := fmt.Sprintf("call-%d-%d", i, j)
			calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "fixture", "arguments": "{}"}})
		}
		rc := ""
		if i < 44 {
			rc = "synthetic chain"
		}
		msgs = append(msgs, map[string]any{"role": "assistant", "content": nil, "reasoning_content": rc, "tool_calls": calls})
		for _, c := range calls {
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": c.(map[string]any)["id"], "content": "fixture result"})
		}
	}
	for i := 0; i < 20; i++ {
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "fixture", "reasoning_content": ""})
	}
	in, _ := json.Marshal(map[string]any{"model": "deepseek-v4.1-flash", "messages": msgs, "max_tokens": 131072, "reasoning_effort": "high"})
	body := mustPrepareBody(t, in, false)
	var d map[string]any
	_ = json.Unmarshal(body, &d)
	out := d["messages"].([]any)
	if len(out) != 1953 {
		t.Fatalf("messages=%d", len(out))
	}
	calls, results, assistants := 0, 0, 0
	for _, raw := range out {
		m := raw.(map[string]any)
		if m["role"] == "tool" {
			results++
		}
		if m["role"] == "assistant" {
			assistants++
			if r, ok := m["reasoning"].(string); !ok || r == "" {
				t.Fatal("assistant missing nonempty reasoning")
			}
			if _, ok := m["reasoning_content"].(string); !ok {
				t.Fatal("reasoning_content not string")
			}
			if c, ok := m["tool_calls"].([]any); ok {
				calls += len(c)
			}
		}
	}
	if calls != 764 || results != 764 || assistants != 757 {
		t.Fatalf("calls/results/assistants=%d/%d/%d", calls, results, assistants)
	}
	if string(mustPrepareBody(t, body, false)) != string(body) {
		t.Fatal("preparation is not idempotent")
	}
}
