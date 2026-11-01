package upstream

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAdjacentAssistantToolsRemainPairedAfterPreparation(t *testing.T) {
	input := map[string]any{"model": "deepseek-v4.1-flash", "messages": []any{msg("user", "hello"), assistantWithCalls("first", "c1"), assistantWithCalls("second", "c2"), toolResult("c1", "one"), toolResult("c2", "two")}}
	before, _ := json.Marshal(input)
	prepared := mustPrepareBody(t, before, false)
	var doc map[string]any
	if err := json.Unmarshal(prepared, &doc); err != nil {
		t.Fatal(err)
	}
	messages := doc["messages"].([]any)
	heads, results := 0, 0
	for i, item := range messages {
		m := item.(map[string]any)
		if m["role"] == "tool" {
			results++
		}
		calls, _ := m["tool_calls"].([]any)
		if len(calls) == 0 {
			continue
		}
		heads++
		if len(calls) != 2 {
			t.Fatalf("split heads still present: %v", messages)
		}
		if m["content"] != "first\nsecond" {
			t.Fatal("assistant text lost")
		}
		for j, call := range calls {
			result := messages[i+1+j].(map[string]any)
			if result["role"] != "tool" || result["tool_call_id"] != call.(map[string]any)["id"] {
				t.Fatal("tool results are not adjacent to merged calls")
			}
		}
	}
	if heads != 1 || results != 2 {
		t.Fatalf("tool context lost: heads=%d results=%d", heads, results)
	}
	// Preparation is idempotent and does not mutate the caller's maps/slices.
	current, _ := json.Marshal(input)
	if string(current) != string(before) {
		t.Fatal("input mutated")
	}
	again := mustPrepareBody(t, prepared, false)
	var twice map[string]any
	_ = json.Unmarshal(again, &twice)
	if !reflect.DeepEqual(doc, twice) {
		t.Fatal("preparation is not idempotent")
	}
}

func TestAdjacentAssistantMergePreservesBoundariesAndMetadata(t *testing.T) {
	first := assistantWithCalls("", "c1")
	second := assistantWithCalls("", "c2")
	first["name"] = "one"
	second["name"] = "two"
	if _, changed := mergeAdjacentAssistantCalls([]any{first, second}); changed {
		t.Fatal("conflicting speakers merged")
	}
	if _, changed := mergeAdjacentAssistantCalls([]any{assistantWithCalls("", "c1"), msg("assistant", "explanation"), assistantWithCalls("", "c2")}); changed {
		t.Fatal("text-only boundary merged")
	}
	a := assistantWithCalls("", "c1")
	a["content"] = []any{map[string]any{"type": "text", "text": "first"}}
	a["reasoning_content"] = "think one"
	b := assistantWithCalls("second", "c2")
	b["reasoning_content"] = "think two"
	out, changed := mergeAdjacentAssistantCalls([]any{a, b})
	if !changed || len(out) != 1 {
		t.Fatal("compatible heads not merged")
	}
	m := out[0].(map[string]any)
	if len(m["content"].([]any)) != 2 || m["reasoning_content"] != "think one\nthink two" {
		t.Fatal("multimodal or reasoning content lost")
	}
}

func TestRequestRejectionDoesNotBecomeAccountRestriction(t *testing.T) {
	for _, tc := range []struct {
		body string
		want ErrKind
	}{
		{`{"code":11128,"msg":"request illegal"}`, ErrClient},
		{`{"code":11140,"msg":"The model provider is rate-limiting requests."}`, ErrSoftRate},
		{`{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity"}}`, ErrAccountDisabled},
	} {
		if got := Classify(403, tc.body); got != tc.want {
			t.Errorf("got %s want %s", got, tc.want)
		}
	}
}
