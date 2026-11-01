// SPDX-FileCopyrightText: 2026 yreidev and contributors (see NOTICE)
// SPDX-FileCopyrightText: 2026 gao-gao-zai and contributors
// SPDX-License-Identifier: AGPL-3.0-only
// Adapted from https://github.com/yreidev/workbuddy2api-panel.

package upstream

import "math"

// The upstream accepts max_tokens. Prefer an explicit canonical field and
// translate only a positive integer alias; absent/unset aliases add no limit.
func translateMaxCompletionTokens(obj map[string]any) {
	alias, exists := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens")
	if _, explicit := obj["max_tokens"]; !exists || explicit {
		return
	}
	switch n := alias.(type) {
	case float64:
		if n > 0 && n < 1<<63 && math.Trunc(n) == n {
			obj["max_tokens"] = int64(n)
		}
	case int:
		if n > 0 {
			obj["max_tokens"] = int64(n)
		}
	case int64:
		if n > 0 {
			obj["max_tokens"] = n
		}
	}
}
