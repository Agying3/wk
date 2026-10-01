package main

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/server"
)

func TestCombinedMetricsAdapterPreservesUsageAndCreditFields(t *testing.T) {
	s, err := metricsstore.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := metricsAdapter{store: s}
	for _, code := range []int{200, 400} {
		err := m.RecordCompletion(server.RequestLog{ID: "WB/unchanged", Status: code, InputTokens: 10, OutputTokens: 4, TotalTokens: 14,
			CacheReadTokens: 3, CacheWriteTokens: 2, ToolCalls: 1, LatencyMillis: 100, TTFBMillis: 0, CreditsConsumed: .25, CreditSource: "estimated"}, true)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 2 || got.Successes != 1 || got.Failures != 1 || got.InputTokens != 20 || got.OutputTokens != 8 || got.TotalTokens != 28 ||
		got.CacheRead != 6 || got.CacheWrite != 4 || got.ToolCalls != 2 || got.LatencyMillis != 200 || got.TTFBSamples != 2 ||
		got.CreditsEstimated != .5 || got.CreditsConsumed != .5 || got.CreditRequests != 2 {
		t.Fatalf("metrics=%+v", got)
	}
	logs, err := m.RecentRequests(10)
	if err != nil || len(logs) != 2 || logs[0].ID != "WB/unchanged" || logs[0].Status != 400 {
		t.Fatalf("logs=%+v err=%v", logs, err)
	}
}

func TestMetricsAdapterPreservesSummaryCoverage(t *testing.T) {
	s, err := metricsstore.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := metricsAdapter{store: s}
	for _, record := range []metricsstore.RequestRecord{
		{ID: "older", CreatedAt: 1000, Model: "other", Status: 200},
		{ID: "one", CreatedAt: 2000, Model: "selected", Status: 200, CreditsConsumed: 1.5, CreditSource: "upstream"},
		{ID: "two", CreatedAt: 3000, Model: "selected", Status: 200, CreditsConsumed: .25, CreditSource: "estimated"},
		{ID: "three", CreatedAt: 3500, Model: "selected", Status: 400, CreditSource: "unknown"},
	} {
		if err := s.RecordRequest(record); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := m.SummarizeRequests(server.RequestLogFilter{SinceUnix: 500, UntilUnix: 4000, Model: "selected", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 3 || summary.CreditsUpstream != 1.5 || summary.CreditsEstimated != .25 || summary.CreditUnknownRequests != 1 || summary.CreditUpstreamRequests != 1 || summary.CreditEstimatedRequests != 1 {
		t.Fatalf("summary fields dropped: %+v", summary)
	}
	if summary.Coverage == nil || summary.Coverage.EarliestRetainedAt != 1000 || summary.Coverage.LatestRetainedAt != 3500 || summary.Coverage.RequestedSince != 500 || summary.Coverage.RequestedUntil != 4000 || !summary.Coverage.StartsBeforeRetained {
		t.Fatalf("coverage fields dropped: %+v", summary.Coverage)
	}
}
