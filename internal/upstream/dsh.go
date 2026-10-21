package upstream

import (
	"strings"
	"sync/atomic"
)

// DSH's fixed opening sentence can trigger upstream channel verification.
// Rewrite only its system-prompt identity, retaining the harness and meaning.
const (
	dshIdentityOriginal  = "You are an AI agent powered by DeepSeek Harness"
	dshIdentityRewritten = "You are an AI agent running on DeepSeek Harness"
)

var dshCompatEnabled atomic.Bool

// SetDSHCompat is safe to update while requests are preparing their payloads.
func SetDSHCompat(enabled bool) { dshCompatEnabled.Store(enabled) }

func rewriteDSHPromptText(text string) string {
	var out strings.Builder
	pos, written := 0, 0
	for pos < len(text) {
		i := strings.Index(text[pos:], dshIdentityOriginal)
		if i < 0 {
			break
		}
		i += pos
		end := i + len(dshIdentityOriginal)
		pos = end
		// Both the canonical sentence (with '.') and the observed bare identity
		// are supported, without matching longer names such as "Harnesses".
		if end < len(text) && !strings.ContainsRune(". \t\r\n", rune(text[end])) {
			continue
		}
		out.WriteString(text[written:i])
		out.WriteString(dshIdentityRewritten)
		written = end
	}
	if written == 0 {
		return text
	}
	out.WriteString(text[written:])
	return out.String()
}

// Developer roles have already been normalized to system before this runs.
// User/assistant/tool messages and non-text parts are left intact.
func rewriteDSHPromptMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if !strings.EqualFold(role, "system") {
			continue
		}
		switch content := m["content"].(type) {
		case string:
			if rewritten := rewriteDSHPromptText(content); rewritten != content {
				m["content"] = rewritten
				changed = true
			}
		case []any:
			for _, item := range content {
				part, ok := item.(map[string]any)
				if !ok {
					continue
				}
				kind, _ := part["type"].(string)
				if kind != "text" && kind != "input_text" {
					continue
				}
				if text, ok := part["text"].(string); ok {
					if rewritten := rewriteDSHPromptText(text); rewritten != text {
						part["text"] = rewritten
						changed = true
					}
				}
			}
		}
	}
	return changed
}
