package metricsstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"workbuddy2api/internal/traffic"
)

// TrafficStore uses the selected metrics database but an independent retention
// policy. No prompts, response text, API keys or proxy URLs enter these tables.
type TrafficStore struct {
	db        *sql.DB
	postgres  bool
	lastPrune time.Time
}

func (s *Store) OpenTrafficStore() (traffic.Store, error) {
	store, err := openTrafficStore(s.db, false)
	if err != nil {
		return nil, err
	}
	return store, nil
}
func (s *PostgresStore) OpenTrafficStore() (traffic.Store, error) {
	store, err := openTrafficStore(s.db, true)
	if err != nil {
		return nil, err
	}
	return store, nil
}

func openTrafficStore(db *sql.DB, postgres bool) (*TrafficStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS traffic_buckets (
			run_id TEXT NOT NULL, minute BIGINT NOT NULL, dimension TEXT NOT NULL,
			entity_id TEXT NOT NULL, name TEXT NOT NULL, data TEXT NOT NULL,
			PRIMARY KEY(run_id, minute, dimension, entity_id))`,
		`CREATE INDEX IF NOT EXISTS traffic_buckets_minute_idx ON traffic_buckets(minute)`,
		`CREATE TABLE IF NOT EXISTS traffic_runs (run_id TEXT PRIMARY KEY, started_ns BIGINT NOT NULL, last_ns BIGINT NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			return nil, fmt.Errorf("initialize traffic store: %w", err)
		}
	}
	return &TrafficStore{db: db, postgres: postgres}, nil
}

func (s *TrafficStore) bind(query string) string {
	if !s.postgres {
		return query
	}
	n := 0
	var b strings.Builder
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *TrafficStore) FirstSeen(ctx context.Context) (time.Time, error) {
	var ns sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MIN(started_ns) FROM traffic_runs`).Scan(&ns)
	if err != nil || !ns.Valid {
		return time.Time{}, err
	}
	return time.Unix(0, ns.Int64), nil
}

func (s *TrafficStore) Save(ctx context.Context, run string, started, now time.Time, buckets []traffic.Bucket) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, s.bind(`INSERT INTO traffic_runs(run_id,started_ns,last_ns) VALUES(?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET last_ns=excluded.last_ns`), run, started.UnixNano(), now.UnixNano())
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, s.bind(`INSERT INTO traffic_buckets(run_id,minute,dimension,entity_id,name,data) VALUES(?,?,?,?,?,?)
		ON CONFLICT(run_id,minute,dimension,entity_id) DO UPDATE SET name=excluded.name,data=excluded.data`))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, b := range buckets {
		raw, err := json.Marshal(b.Counters)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, run, b.Minute, b.Dimension, b.ID, b.Name, string(raw)); err != nil {
			return err
		}
	}
	prune := s.lastPrune.IsZero() || now.Sub(s.lastPrune) >= time.Hour
	if prune {
		cutoff := now.Add(-traffic.Retention)
		if _, err := tx.ExecContext(ctx, s.bind(`DELETE FROM traffic_buckets WHERE minute < ?`), cutoff.Unix()/60*60); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.bind(`DELETE FROM traffic_runs WHERE last_ns < ?`), cutoff.UnixNano()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if prune {
		s.lastPrune = now
	}
	return nil
}

var trafficFields = []string{"requests", "completed", "successes", "failures", "input_tokens", "output_tokens", "total_tokens",
	"cache_read_tokens", "cache_write_tokens", "cache_input_tokens", "cache_samples", "tool_calls", "ttfb_millis_sum", "ttfb_samples", "latency_millis_sum",
	"credits_upstream", "credits_estimated", "credit_unknown_requests", "concurrency_seconds", "peak_concurrency"}

func (s *TrafficStore) Query(ctx context.Context, since, until int64, overlayRun string, overlayMinute int64) ([]traffic.Bucket, error) {
	// Aggregate in SQL, returning one row per entity regardless of window size.
	// The live tracker overlays its own buffered minutes, avoiding double count
	// even if they were already saved before the latest request completed.
	parts := []string{"dimension", "entity_id", "MAX(name)"}
	for _, field := range trafficFields {
		expr := "COALESCE(json_extract(data, '$." + field + "'), 0)"
		if s.postgres {
			typ := "bigint"
			switch field {
			case "ttfb_millis_sum", "latency_millis_sum", "credits_upstream", "credits_estimated", "concurrency_seconds":
				typ = "double precision"
			}
			expr = "COALESCE(CAST(CAST(data AS jsonb)->>'" + field + "' AS " + typ + "), 0)"
		}
		agg := "SUM"
		if field == "peak_concurrency" {
			agg = "MAX"
		}
		parts = append(parts, agg+"("+expr+")")
	}
	query := "SELECT " + strings.Join(parts, ",") + ` FROM traffic_buckets WHERE minute >= ? AND minute <= ?
		AND NOT (run_id = ? AND minute >= ?) GROUP BY dimension,entity_id`
	rows, err := s.db.QueryContext(ctx, s.bind(query), since, until, overlayRun, overlayMinute)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []traffic.Bucket{}
	for rows.Next() {
		var b traffic.Bucket
		if err := rows.Scan(&b.Dimension, &b.ID, &b.Name, &b.Requests, &b.Completed, &b.Successes, &b.Failures,
			&b.InputTokens, &b.OutputTokens, &b.TotalTokens, &b.CacheReadTokens, &b.CacheWriteTokens, &b.CacheInputTokens, &b.CacheSamples, &b.ToolCalls,
			&b.TTFBMillis, &b.TTFBSamples, &b.LatencyMillis, &b.CreditsUpstream, &b.CreditsEstimated, &b.CreditUnknownRequests,
			&b.ConcurrencySeconds, &b.PeakConcurrency); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, rows.Err()
}
