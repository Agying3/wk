package metricsstore

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSummaryCoverageAndCreditSources(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "metrics.db"), Retention{MaxRows: 3, MaxAge: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Unix()
	for i, source := range []string{"upstream", "upstream", "estimated", "unknown"} {
		if err := s.RecordRequest(RequestRecord{ID: "same", CreatedAt: now - 400 + int64(i)*100,
			Model: "selected", Status: 200, CreditsConsumed: float64(i + 1), CreditSource: source,
			InputTokens: 100, CacheReadTokens: 80}); err != nil {
			t.Fatal(err)
		}
	}
	if err := trimRequestLogs(s.db, s.RetentionPolicy()); err != nil {
		t.Fatal(err)
	}
	for _, dialect := range []string{"?", "$"} {
		// SQLite also accepts $N parameters, exercising the exact shared SQL
		// and argument ordering used by Postgres without a production database.
		summary, err := summarizeRequests(s.db, RequestFilter{SinceUnix: now - 1000, UntilUnix: now, Model: "selected", Limit: 1}, dialect, s.RetentionPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if summary.Requests != 3 || summary.CreditsUpstream != 2 || summary.CreditsEstimated != 3 || summary.CreditsConsumed != 9 {
			t.Fatalf("dialect=%s summary=%+v", dialect, summary)
		}
		if summary.CreditUpstreamRequests != 1 || summary.CreditEstimatedRequests != 1 || summary.CreditUnknownRequests != 1 {
			t.Fatalf("credit coverage=%+v", summary)
		}
		c := summary.Coverage
		if c == nil || c.EarliestRetainedAt != now-300 || c.LatestRetainedAt != now-100 || !c.StartsBeforeRetained || c.RetentionRows != 3 || c.RetentionSeconds != 30*86400 {
			t.Fatalf("retained coverage=%+v", c)
		}
	}
	// A model with no matches still gets global coverage, not a fabricated
	// boundary from the model's first row or an empty database.
	empty, err := s.SummarizeRequests(RequestFilter{SinceUnix: now - 200, Model: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Requests != 0 || empty.CreditUnknownRequests != 0 || empty.Coverage.EarliestRetainedAt != now-300 || empty.Coverage.StartsBeforeRetained {
		t.Fatalf("empty filtered summary=%+v coverage=%+v", empty, empty.Coverage)
	}
}

func TestSummaryEmptyHistoryHasNoFalseCoverageWarning(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	summary, err := s.SummarizeRequests(RequestFilter{SinceUnix: 1, UntilUnix: 2})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 0 || summary.Coverage == nil || summary.Coverage.EarliestRetainedAt != 0 || summary.Coverage.StartsBeforeRetained {
		t.Fatalf("empty history=%+v coverage=%+v", summary, summary.Coverage)
	}
}
