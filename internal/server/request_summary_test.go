package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type unavailableSummaryStore struct{ fakeQueryStore }

func (s *unavailableSummaryStore) SummarizeRequests(RequestLogFilter) (RequestSummary, error) {
	return RequestSummary{}, errors.New("private database connection details")
}

func TestRequestsSummaryFailureIsExplicit(t *testing.T) {
	store := &unavailableSummaryStore{}
	h := newTestHandler(t, Config{RequestLogStore: store})
	for _, route := range []string{"/requests", "/v1/requests"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route+"?include=summary&since=100", nil))
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "request_summary_unavailable") {
			t.Fatalf("route=%s status=%d body=%s", route, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "private database") || strings.Contains(rec.Body.String(), `"summary"`) {
			t.Fatalf("failed summary leaked details or appeared successful: %s", rec.Body)
		}
	}
	// Requests without an aggregate remain usable when aggregation fails.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/requests?limit=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("rows-only status=%d", rec.Code)
	}
}
