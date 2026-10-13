package upstream

import (
	"strings"
	"testing"
)

func TestAggregatePreservesPartialUsageAndEmptyTail(t *testing.T) {
	stream := `data: {"choices":[{"delta":{"content":"ok"}}],"usage":{"input_tokens":1000,"prompt_cache_hit_tokens":800}}` + "\n\n" +
		`data: {"usage":{"output_tokens":20}}` + "\n\n" + `data: {"usage":{"credit":0.1}}` + "\n\n" + `data: {"usage":{}}` + "\n\ndata: [DONE]\n\n"
	resp, err := AggregateWithID(strings.NewReader(stream), "")
	if err != nil {
		t.Fatal(err)
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(1000) || usage["prompt_cache_hit_tokens"] != float64(800) || usage["output_tokens"] != float64(20) {
		t.Fatalf("usage=%v", usage)
	}
}

func TestMergeUsageExplicitZeroReplacesCacheAliases(t *testing.T) {
	old := map[string]any{"input_tokens": 1000., "prompt_cache_hit_tokens": 800., "input_tokens_details": map[string]any{"cached_tokens": 800., "cache_write_tokens": 200.}}
	next := map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": 0.}}
	merged := MergeUsage(old, next)
	if _, ok := merged["prompt_cache_hit_tokens"]; ok {
		t.Fatal("old flat cache survived explicit nested zero")
	}
	details := merged["input_tokens_details"].(map[string]any)
	if _, ok := details["cached_tokens"]; ok {
		t.Fatal("old nested cache survived")
	}
	if details["cache_write_tokens"] != 200. {
		t.Fatal("unrelated usage field lost")
	}
}
