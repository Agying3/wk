package metricsstore

import (
	"context"
	"database/sql"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/traffic"
)

func TestTrafficStoreInitializationFailureReturnsNil(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, open := range []func() (traffic.Store, error){(&Store{db: db}).OpenTrafficStore, (&PostgresStore{db: db}).OpenTrafficStore} {
		backend, err := open()
		if err == nil || backend != nil {
			t.Fatalf("unavailable store must allow memory fallback: store=%v error=%v", backend, err)
		}
	}
}

func TestTrafficSQLiteIdempotenceRestartAndIndependentRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	db, err := Open(path, Retention{MaxRows: 1})
	if err != nil {
		t.Fatal(err)
	}
	store, err := db.OpenTrafficStore()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(600, 0)
	tracker := traffic.NewWithClock(store, func() time.Time { return now })
	l := tracker.Begin(traffic.Entity{Dimension: "key", ID: "public-key-id", Name: "测试 key"})
	now = time.Unix(610, 0)
	l.End(traffic.Counters{Completed: 1, Successes: 1, CreditsUpstream: 0.123, InputTokens: 10, CacheInputTokens: 10, CacheReadTokens: 2, CacheSamples: 1, TTFBSamples: 1, TTFBMillis: 0.2})
	for i := 0; i < 2; i++ {
		if err := tracker.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// Clearing request logs must not remove traffic buckets.
	if _, err := db.db.Exec(`DELETE FROM request_logs`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err = db.OpenTrafficStore()
	if err != nil {
		t.Fatal(err)
	}
	now = time.Unix(615, 0)
	restarted := traffic.NewWithClock(store, func() time.Time { return now })
	l = restarted.Begin(traffic.Entity{Dimension: "key", ID: "public-key-id", Name: "测试 key"})
	now = time.Unix(620, 0)
	l.End(traffic.Counters{Completed: 1, Failures: 1, CreditUnknownRequests: 1})
	if err := restarted.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := restarted.Snapshot(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Rows) != 1 {
		t.Fatalf("rows: %+v", s)
	}
	r := s.Rows[0]
	if r.Requests != 2 || r.Completed != 2 || r.Successes != 1 || r.Failures != 1 || r.CurrentConcurrency != 0 || r.RPM != 1 || r.PeakConcurrency != 1 || r.CreditUnknownRequests != 1 {
		t.Fatalf("restart counters: %+v", r)
	}
	if math.Abs(r.ConcurrencySeconds-15) > 1e-9 || math.Abs(r.CreditsConsumed-0.123) > 1e-9 || *r.AverageTTFB != 0.2 {
		t.Fatalf("fractional counters: %+v", r)
	}
	if r.CacheRate == nil || *r.CacheRate != 20 || r.CacheSamples != 1 {
		t.Fatalf("persisted cache observations: %+v", r)
	}
	if s.CollectionStartedAt != 600 || s.Persistence != "database" {
		t.Fatalf("metadata: %+v", s)
	}
	var count int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM traffic_buckets`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("run isolation rows=%d err=%v", count, err)
	}
}

func TestTrafficSQLWindowOverlayAndPruning(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend, err := db.OpenTrafficStore()
	if err != nil {
		t.Fatal(err)
	}
	store := backend.(*TrafficStore)
	now := time.Unix(600, 0)
	b := traffic.Bucket{Entity: traffic.Entity{Dimension: "total", ID: "all"}, Minute: 600, Counters: traffic.Counters{Requests: 3, PeakConcurrency: 2}}
	if err := store.Save(context.Background(), "run", now, now, []traffic.Bucket{b}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Query(context.Background(), 600, 660, "run", 600)
	if err != nil || len(rows) != 0 {
		t.Fatalf("overlay=%+v err=%v", rows, err)
	}
	rows, err = store.Query(context.Background(), 660, 720, "another", 660)
	if err != nil || len(rows) != 0 {
		t.Fatalf("window=%+v err=%v", rows, err)
	}
	rows, err = store.Query(context.Background(), 600, 660, "another", 600)
	if err != nil || len(rows) != 1 || rows[0].Requests != 3 {
		t.Fatalf("query=%+v err=%v", rows, err)
	}
	now = now.Add(traffic.Retention + time.Hour)
	if err := store.Save(context.Background(), "new-run", now, now, nil); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Query(context.Background(), 0, now.Unix(), "x", now.Unix())
	if err != nil || len(rows) != 0 {
		t.Fatalf("prune=%+v err=%v", rows, err)
	}
}

func TestTrafficPostgres(t *testing.T) {
	dsn := os.Getenv("WK_TRAFFIC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires an isolated local PostgreSQL fixture")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Path != "/wb2a_traffic_test" {
		t.Fatal("traffic integration test requires the dedicated loopback test database")
	}
	db, err := OpenPostgres(dsn, 4, 2, time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := db.OpenTrafficStore()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(600, 0)
	tracker := traffic.NewWithClock(store, func() time.Time { return now })
	l := tracker.Begin(traffic.Entity{Dimension: "proxy", ID: "node-id", Name: "节点"})
	now = time.Unix(610, 0)
	l.End(traffic.Counters{Completed: 1, Successes: 1, CreditsUpstream: 0.123, InputTokens: 10, CacheInputTokens: 10, CacheReadTokens: 2, CacheSamples: 1, TTFBSamples: 1, TTFBMillis: 0.2})
	for i := 0; i < 2; i++ {
		if err := tracker.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	now = time.Unix(660, 0)
	s, err := traffic.NewWithClock(store, func() time.Time { return now }).Snapshot(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Rows) != 1 {
		t.Fatalf("PG rows: %+v", s.Rows)
	}
	r := s.Rows[0]
	if r.Requests != 1 || r.Completed != 1 || r.PeakConcurrency != 1 || r.InputTokens != 10 || r.TTFBSamples != 1 || math.Abs(r.CreditsConsumed-0.123) > 1e-9 || math.Abs(r.ConcurrencySeconds-10) > 1e-9 {
		t.Fatalf("PG counters: %+v", r)
	}
	if r.CacheRate == nil || *r.CacheRate != 20 || r.CacheSamples != 1 {
		t.Fatalf("PG cache observations: %+v", r)
	}
}
