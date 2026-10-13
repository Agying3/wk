package traffic

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func rowFor(t *testing.T, snapshot Snapshot, dimension, id string) Row {
	t.Helper()
	for _, r := range snapshot.Rows {
		if r.Dimension == dimension && r.ID == id {
			return r
		}
	}
	t.Fatalf("missing %s/%s in %+v", dimension, id, snapshot)
	return Row{}
}
func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-8 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTimeWeightedConcurrencyAndWindowDenominator(t *testing.T) {
	now := time.Unix(600, 0)
	tracker := NewWithClock(nil, func() time.Time { return now })
	entities := []Entity{{Dimension: "total", ID: "all"}, {Dimension: "key", ID: "k"}}
	a := tracker.Begin(entities...)
	now = now.Add(5 * time.Second)
	b := tracker.Begin(entities...)
	now = now.Add(5 * time.Second)
	a.End(Counters{Completed: 1, Successes: 1, InputTokens: 100, CacheReadTokens: 25, CacheInputTokens: 100, CacheSamples: 1, TTFBSamples: 1, TTFBMillis: 123, LatencyMillis: 10000, CreditsUpstream: 0.1})
	a.End(Counters{Completed: 1, Failures: 1}) // duplicate completion is harmless
	now = now.Add(10 * time.Second)
	b.End(Counters{Completed: 1, Failures: 1, LatencyMillis: 15000, CreditUnknownRequests: 1})
	now = time.Unix(660, 0)
	snapshot, err := tracker.Snapshot(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	r := rowFor(t, snapshot, "total", "all")
	if r.Requests != 2 || r.Completed != 2 || r.Successes != 1 || r.Failures != 1 || r.PeakConcurrency != 2 || r.CurrentConcurrency != 0 || r.RPM != 1 {
		t.Fatalf("bad counters: %+v", r)
	}
	// The start at 600 is outside (now-60,now], the one at 605 is inside.
	near(t, r.AverageRPM, 2)
	near(t, r.ConcurrencySeconds, 25)
	near(t, r.AverageConcurrency, 25.0/60)
	near(t, *r.SuccessRate, 50)
	near(t, *r.CacheRate, 25)
	near(t, *r.AverageTTFB, 123)
	near(t, *r.AverageLatency, 12500)
	near(t, r.CreditsConsumed, 0.1)
	key := rowFor(t, snapshot, "key", "k")
	near(t, key.AverageConcurrency, r.AverageConcurrency)
}

func TestLongActiveStreamAcrossMinutesAndRepeatedPolling(t *testing.T) {
	now := time.Unix(600, 0)
	tracker := NewWithClock(nil, func() time.Time { return now })
	now = time.Unix(630, 0)
	lease := tracker.Begin(Entity{Dimension: "account", ID: "a"})
	now = time.Unix(750, 0)
	for i := 0; i < 3; i++ {
		s, err := tracker.Snapshot(context.Background(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		r := rowFor(t, s, "account", "a")
		if r.Requests != 0 || r.CurrentConcurrency != 1 || r.PeakConcurrency != 1 || r.RPM != 0 {
			t.Fatalf("long stream: %+v", r)
		}
		// Window starts at the minute boundary 660, ending at 750.
		near(t, r.ConcurrencySeconds, 90)
		near(t, r.AverageConcurrency, 1)
	}
	lease.End(Counters{Completed: 1, Successes: 1})
	s, _ := tracker.Snapshot(context.Background(), time.Minute)
	r := rowFor(t, s, "account", "a")
	if r.Completed != 1 || r.CurrentConcurrency != 0 {
		t.Fatalf("completion: %+v", r)
	}
}

type memoryStore struct {
	buckets map[string]Bucket
	first   time.Time
	fail    bool
}

func (s *memoryStore) FirstSeen(context.Context) (time.Time, error) { return s.first, nil }
func (s *memoryStore) Save(_ context.Context, run string, started, _ time.Time, buckets []Bucket) error {
	if s.fail {
		return errors.New("disk unavailable")
	}
	if s.first.IsZero() {
		s.first = started
	}
	if s.buckets == nil {
		s.buckets = map[string]Bucket{}
	}
	for _, b := range buckets {
		s.buckets[run+"/"+b.Dimension+"/"+b.ID+"/"+time.Unix(b.Minute, 0).String()] = b
	}
	return nil
}
func (s *memoryStore) Query(_ context.Context, since, until int64, run string, overlay int64) ([]Bucket, error) {
	result := []Bucket{}
	for key, b := range s.buckets {
		if b.Minute >= since && b.Minute <= until && !(len(key) >= len(run) && key[:len(run)] == run && b.Minute >= overlay) {
			result = append(result, b)
		}
	}
	return result, nil
}

func TestFlushOverlayRestartAndFailedWriteRetry(t *testing.T) {
	now := time.Unix(600, 0)
	store := &memoryStore{}
	tracker := NewWithClock(store, func() time.Time { return now })
	lease := tracker.Begin(Entity{Dimension: "total", ID: "all"})
	now = time.Unix(620, 0)
	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = time.Unix(650, 0)
	lease.End(Counters{Completed: 1, Successes: 1})
	s, _ := tracker.Snapshot(context.Background(), time.Hour)
	r := rowFor(t, s, "total", "all")
	if r.Requests != 1 || r.Completed != 1 {
		t.Fatalf("overlay duplicated or stale: %+v", r)
	}
	near(t, r.ConcurrencySeconds, 50)
	store.fail = true
	now = time.Unix(670, 0)
	if err := tracker.Flush(context.Background()); err == nil {
		t.Fatal("expected persistence failure")
	}
	s, _ = tracker.Snapshot(context.Background(), time.Hour)
	near(t, rowFor(t, s, "total", "all").ConcurrencySeconds, 50)
	store.fail = false
	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, _ = tracker.Snapshot(context.Background(), time.Hour)
	r = rowFor(t, s, "total", "all")
	if r.Requests != 1 || r.Completed != 1 {
		t.Fatalf("retry duplicated: %+v", r)
	}
	now = time.Unix(675, 0)
	restarted := NewWithClock(store, func() time.Time { return now })
	b := restarted.Begin(Entity{Dimension: "total", ID: "all"})
	now = time.Unix(680, 0)
	b.End(Counters{Completed: 1, Failures: 1})
	s, _ = restarted.Snapshot(context.Background(), time.Hour)
	r = rowFor(t, s, "total", "all")
	if r.Requests != 2 || r.Completed != 2 || r.PeakConcurrency != 1 || r.CurrentConcurrency != 0 || r.RPM != 1 {
		t.Fatalf("restart: %+v", r)
	}
	near(t, r.ConcurrencySeconds, 55)
	if s.CollectionStartedAt != 600 || !s.Partial {
		t.Fatalf("coverage: %+v", s)
	}
}

func TestMultipleMembershipDedupAndConcurrentRelease(t *testing.T) {
	tracker := New(nil)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := tracker.Begin(Entity{Dimension: "account", ID: "a"}, Entity{Dimension: "account_group", ID: "g1"}, Entity{Dimension: "account_group", ID: "g1"}, Entity{Dimension: "account_group", ID: "g2"})
			l.End(Counters{Completed: 1, Successes: 1})
			l.End(Counters{Completed: 1})
		}()
	}
	wg.Wait()
	s, err := tracker.Snapshot(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"g1", "g2"} {
		r := rowFor(t, s, "account_group", id)
		if r.Requests != 100 || r.Completed != 100 || r.CurrentConcurrency != 0 {
			t.Fatalf("membership: %+v", r)
		}
	}
}

func TestIdleRetentionAndMissingSamples(t *testing.T) {
	now := time.Unix(600, 0)
	tracker := NewWithClock(nil, func() time.Time { return now })
	tracker.Begin(Entity{Dimension: "total", ID: "all"}).End(Counters{Completed: 1, Failures: 1})
	s, _ := tracker.Snapshot(context.Background(), time.Minute)
	r := rowFor(t, s, "total", "all")
	if r.AverageTTFB != nil || r.CacheRate != nil {
		t.Fatalf("missing samples became zero: %+v", r)
	}
	now = now.Add(Retention + time.Hour)
	s, _ = tracker.Snapshot(context.Background(), Retention)
	if len(s.Rows) != 0 || len(tracker.buckets) != 0 || len(tracker.states) != 0 {
		t.Fatal("expired idle data was retained")
	}
}

func TestCacheRateExcludesRequestsWithoutCacheObservations(t *testing.T) {
	now := time.Unix(600, 0)
	tracker := NewWithClock(nil, func() time.Time { return now })
	entity := Entity{Dimension: "total", ID: "all"}
	tracker.Begin(entity).End(Counters{Completed: 1, Successes: 1, InputTokens: 1000})
	s, _ := tracker.Snapshot(context.Background(), time.Hour)
	if rowFor(t, s, "total", "all").CacheRate != nil {
		t.Fatal("unknown cache usage must not appear as 0%")
	}
	tracker.Begin(entity).End(Counters{Completed: 1, Successes: 1, InputTokens: 100, CacheReadTokens: 60, CacheInputTokens: 100, CacheSamples: 1})
	s, _ = tracker.Snapshot(context.Background(), time.Hour)
	r := rowFor(t, s, "total", "all")
	near(t, *r.CacheRate, 60)
	if r.InputTokens != 1100 || r.CacheSamples != 1 {
		t.Fatalf("cache coverage lost total usage: %+v", r)
	}
	tracker.Begin(entity).End(Counters{Completed: 1, Successes: 1, InputTokens: 100, CacheInputTokens: 100, CacheSamples: 1})
	s, _ = tracker.Snapshot(context.Background(), time.Hour)
	near(t, *rowFor(t, s, "total", "all").CacheRate, 30)
}
