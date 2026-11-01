package upstream

import "reflect"

// Some Chat clients split one parallel tool-call turn into consecutive
// assistant messages. Merge those heads before pairing results; otherwise each
// head appears to interrupt the other's result block. Do not merge across any
// other role, a text-only assistant, or conflicting message metadata.
func mergeAdjacentAssistantCalls(messages []any) ([]any, bool) {
	var out []any
	changed := false
	for i, raw := range messages {
		current, ok := raw.(map[string]any)
		calls, _ := current["tool_calls"].([]any)
		if !ok || current["role"] != "assistant" || len(calls) == 0 || i == 0 {
			out = append(out, raw)
			continue
		}
		previous, _ := out[len(out)-1].(map[string]any)
		oldCalls, _ := previous["tool_calls"].([]any)
		if previous["role"] != "assistant" || len(oldCalls) == 0 {
			out = append(out, raw)
			continue
		}
		merged := make(map[string]any, len(previous)+len(current))
		for key, value := range previous {
			merged[key] = value
		}
		compatible := true
		for key, value := range current {
			if key == "tool_calls" {
				continue
			}
			old, exists := merged[key]
			if !exists || old == nil {
				merged[key] = value
				continue
			}
			if value == nil {
				continue
			}
			if key == "content" || key == "reasoning_content" {
				joined, ok := joinAssistantContent(old, value)
				if !ok {
					compatible = false
					break
				}
				merged[key] = joined
			} else if !reflect.DeepEqual(old, value) {
				compatible = false
				break
			}
		}
		if !compatible {
			out = append(out, raw)
			continue
		}
		merged["tool_calls"] = append(append(make([]any, 0, len(oldCalls)+len(calls)), oldCalls...), calls...)
		out[len(out)-1] = merged
		changed = true
	}
	if !changed {
		return messages, false
	}
	return out, true
}

func joinAssistantContent(first, second any) (any, bool) {
	if a, ok := first.(string); ok {
		if b, ok := second.(string); ok {
			if a == "" {
				return b, true
			}
			if b == "" {
				return a, true
			}
			return a + "\n" + b, true
		}
	}
	parts := func(value any) ([]any, bool) {
		switch v := value.(type) {
		case string:
			if v == "" {
				return nil, true
			}
			return []any{map[string]any{"type": "text", "text": v}}, true
		case []any:
			return v, true
		default:
			return nil, false
		}
	}
	a, okA := parts(first)
	b, okB := parts(second)
	if !okA || !okB {
		return nil, false
	}
	return append(append(make([]any, 0, len(a)+len(b)), a...), b...), true
}
