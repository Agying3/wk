package metricsstore

import "database/sql"

// Both backends use the same aggregate and one database snapshot. Retained
// bounds are index lookups, not additional scans of the unfiltered log table.
func summarizeRequests(db *sql.DB, filter RequestFilter, placeholder string, retention Retention) (RequestSummary, error) {
	where, args := requestFilterWhere(filter, placeholder)
	row := db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status >= 200 AND status < 300 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status >= 200 AND status < 300 THEN 0 ELSE 1 END), 0),
		COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0), COALESCE(SUM(tool_calls), 0),
		COALESCE(SUM(ttfb_millis), 0), COALESCE(SUM(CASE WHEN ttfb_millis > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(latency_millis), 0), COALESCE(SUM(credits_consumed), 0),
		COALESCE(SUM(CASE WHEN credit_source = 'upstream' THEN credits_consumed ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN credit_source = 'estimated' THEN credits_consumed ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN credit_source = 'upstream' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN credit_source = 'estimated' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN credit_source IN ('upstream', 'estimated') THEN 0 ELSE 1 END), 0),
		COALESCE((SELECT created_at FROM request_logs ORDER BY created_at ASC LIMIT 1), 0),
		COALESCE((SELECT created_at FROM request_logs ORDER BY created_at DESC LIMIT 1), 0)
		FROM request_logs`+where, args...)
	var summary RequestSummary
	coverage := &RequestLogCoverage{
		RequestedSince: filter.SinceUnix, RequestedUntil: filter.UntilUnix,
		RetentionRows: retention.rowsCap(), RetentionSeconds: int64(retention.MaxAge.Seconds()),
	}
	if err := row.Scan(&summary.Requests, &summary.Successes, &summary.Failures,
		&summary.InputTokens, &summary.OutputTokens, &summary.TotalTokens,
		&summary.CacheReadTokens, &summary.CacheWriteTokens, &summary.ToolCalls,
		&summary.TTFBMillisSum, &summary.TTFBSamples, &summary.LatencyMillisSum,
		&summary.CreditsConsumed, &summary.CreditsUpstream, &summary.CreditsEstimated,
		&summary.CreditUpstreamRequests, &summary.CreditEstimatedRequests, &summary.CreditUnknownRequests,
		&coverage.EarliestRetainedAt, &coverage.LatestRetainedAt); err != nil {
		return RequestSummary{}, err
	}
	coverage.StartsBeforeRetained = filter.SinceUnix > 0 && coverage.EarliestRetainedAt > filter.SinceUnix
	summary.Coverage = coverage
	return summary, nil
}
