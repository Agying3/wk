package promptguard

import (
	"strings"
	"testing"
)

func TestPreviewRuleBoundariesCandidatesAndNoMutation(t *testing.T) {
	rule := Rule{Match: "Harness", RequireEndBoundary: true, Variants: []Variant{{ID: "a", Replacement: "Platform", Weight: 1}, {ID: "b", Replacement: "", Weight: 0}}}
	for _, tc := range []struct{ id, want string }{{"a", "Harnesses Platform. Platform"}, {"b", "Harnesses . "}} {
		got, err := PreviewRule(rule, tc.id, "Harnesses Harness. Harness")
		if err != nil || got.Matches != 2 || got.Output != tc.want || got.RuleEnabled {
			t.Fatalf("%s: %+v %v", tc.id, got, err)
		}
	}
	if rule.Replacement != "" || rule.Enabled {
		t.Fatal("preview changed rule")
	}
	if _, err := PreviewRule(rule, "missing", "Harness"); err == nil {
		t.Fatal("unknown candidate accepted")
	}
	got, err := PreviewRule(rule, "a", "HARNESS")
	if err != nil || got.Matches != 0 || got.Output != "HARNESS" {
		t.Fatalf("case insensitive preview: %+v %v", got, err)
	}
}

func TestPreviewRuleBoundsExpansionAndRewritesOnce(t *testing.T) {
	got, err := PreviewRule(Rule{Match: "a", Replacement: "aa"}, "", "aa")
	if err != nil || got.Output != "aaaa" || got.Matches != 2 {
		t.Fatalf("recursive result: %+v %v", got, err)
	}
	if _, err := PreviewRule(Rule{Match: "a", Replacement: strings.Repeat("b", 1024)}, "", strings.Repeat("a", 1025)); err == nil {
		t.Fatal("large expansion accepted")
	}
	if _, err := PreviewRule(Rule{Match: "a"}, "", strings.Repeat("b", 1<<20+1)); err == nil {
		t.Fatal("large sample accepted")
	}
}
