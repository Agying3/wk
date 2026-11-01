package upstream

import (
	"encoding/json"
	"testing"
)

func TestPreparedTokenLimitPreservesRequestedBudget(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         any
		present      bool
	}{
		{"alias", `"max_completion_tokens":128`, float64(128), true},
		{"canonical_wins", `"max_completion_tokens":128,"max_tokens":64`, float64(64), true},
		{"canonical_null_wins", `"max_completion_tokens":128,"max_tokens":null`, nil, true},
		{"no_limit", `"messages":[]`, nil, false},
		{"unset_null", `"max_completion_tokens":null`, nil, false},
		{"unset_zero", `"max_completion_tokens":0`, nil, false},
		{"negative", `"max_completion_tokens":-1`, nil, false},
		{"fraction", `"max_completion_tokens":1.5`, nil, false},
		{"string", `"max_completion_tokens":"128"`, nil, false},
		{"overflow", `"max_completion_tokens":9223372036854775808`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := mustPrepareBody(t, []byte(`{"model":"deepseek-v4.1-flash",`+tc.fields+`}`), false)
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if _, exists := got["max_completion_tokens"]; exists {
				t.Fatal("unsupported alias reached upstream body")
			}
			limit, exists := got["max_tokens"]
			if exists != tc.present || limit != tc.want {
				t.Fatalf("output budget=%v present=%v want=%v present=%v", limit, exists, tc.want, tc.present)
			}
			if again := mustPrepareBody(t, body, false); string(again) != string(body) {
				t.Fatal("token budget changed on repeated preparation")
			}
		})
	}
}
