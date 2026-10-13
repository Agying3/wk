// Package traffic measures model traffic independently of request-log retention.
// Client requests and upstream attempts occupy different dimensions. Minute
// buckets integrate concurrency over time; polling frequency never affects it.
package traffic

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

const Retention = 30 * 24 * time.Hour

type Entity struct {
	Dimension string `json:"dimension"`
	ID        string `json:"id"`
	Name      string `json:"name"`
}

// Counters records arrivals in their start minute, outcomes/usage in their
// completion minute, and occupied seconds in every minute they overlap.
type Counters struct {
	Requests              int64   `json:"requests"`
	Completed             int64   `json:"completed"`
	Successes             int64   `json:"successes"`
	Failures              int64   `json:"failures"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	TotalTokens           int64   `json:"total_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	CacheWriteTokens      int64   `json:"cache_write_tokens"`
	CacheInputTokens      int64   `json:"cache_input_tokens"`
	CacheSamples          int64   `json:"cache_samples"`
	ToolCalls             int64   `json:"tool_calls"`
	TTFBMillis            float64 `json:"ttfb_millis_sum"`
	TTFBSamples           int64   `json:"ttfb_samples"`
	LatencyMillis         float64 `json:"latency_millis_sum"`
	CreditsUpstream       float64 `json:"credits_upstream"`
	CreditsEstimated      float64 `json:"credits_estimated"`
	CreditUnknownRequests int64   `json:"credit_unknown_requests"`
	ConcurrencySeconds    float64 `json:"concurrency_seconds"`
	PeakConcurrency       int64   `json:"peak_concurrency"`
}

func (c *Counters) Add(v Counters) {
	c.Requests += v.Requests
	c.Completed += v.Completed
	c.Successes += v.Successes
	c.Failures += v.Failures
	c.InputTokens += v.InputTokens
	c.OutputTokens += v.OutputTokens
	c.TotalTokens += v.TotalTokens
	c.CacheReadTokens += v.CacheReadTokens
	c.CacheWriteTokens += v.CacheWriteTokens
	c.CacheInputTokens += v.CacheInputTokens
	c.CacheSamples += v.CacheSamples
	c.ToolCalls += v.ToolCalls
	c.TTFBMillis += v.TTFBMillis
	c.TTFBSamples += v.TTFBSamples
	c.LatencyMillis += v.LatencyMillis
	c.CreditsUpstream += v.CreditsUpstream
	c.CreditsEstimated += v.CreditsEstimated
	c.CreditUnknownRequests += v.CreditUnknownRequests
	c.ConcurrencySeconds += v.ConcurrencySeconds
	c.PeakConcurrency = max(c.PeakConcurrency, v.PeakConcurrency)
}

type Bucket struct {
	Entity
	Counters
	Minute int64
}

// Store saves absolute bucket snapshots. A retry is idempotent, and distinct
// process runs cannot overwrite the same minute after a restart.
type Store interface {
	FirstSeen(context.Context) (time.Time, error)
	Save(context.Context, string, time.Time, time.Time, []Bucket) error
	Query(context.Context, int64, int64, string, int64) ([]Bucket, error)
}

type Row struct {
	Entity
	Counters
	RPM                int      `json:"rpm"`
	AverageRPM         float64  `json:"avg_rpm"`
	CurrentConcurrency int64    `json:"current_concurrency"`
	AverageConcurrency float64  `json:"avg_concurrency"`
	SuccessRate        *float64 `json:"success_rate"`
	CacheRate          *float64 `json:"cache_rate"`
	AverageTTFB        *float64 `json:"avg_ttfb_ms"`
	AverageLatency     *float64 `json:"avg_latency_ms"`
	CreditsConsumed    float64  `json:"credits_consumed"`
}

type Snapshot struct {
	Since               int64  `json:"since"`
	Until               int64  `json:"until"`
	CollectionStartedAt int64  `json:"collection_started_at"`
	EffectiveSince      int64  `json:"effective_since"`
	Partial             bool   `json:"partial"`
	Persistence         string `json:"persistence"`
	RetentionDays       int    `json:"retention_days"`
	Rows                []Row  `json:"rows"`
}

type entityKey struct{ dimension, id string }
type bucketKey struct {
	entityKey
	minute int64
}
type state struct {
	entity       Entity
	active       int64
	last         time.Time
	lastActivity time.Time
	starts       []time.Time
}

type Tracker struct {
	mu                 sync.Mutex
	flushMu            sync.Mutex
	store              Store
	now                func() time.Time
	runID              string
	started, firstSeen time.Time
	states             map[entityKey]*state
	buckets            map[bucketKey]*Bucket
}

func New(store Store) *Tracker { return NewWithClock(store, time.Now) }

func NewWithClock(store Store, now func() time.Time) *Tracker {
	started := now()
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		panic(err)
	}
	t := &Tracker{store: store, now: now, runID: fmt.Sprintf("%x", id), started: started, firstSeen: started,
		states: make(map[entityKey]*state), buckets: make(map[bucketKey]*Bucket)}
	if store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		first, err := store.FirstSeen(ctx)
		if err != nil {
			log.Printf("traffic history metadata unavailable: %v", err)
		} else if !first.IsZero() && first.Before(started) {
			t.firstSeen = first
		}
	}
	return t
}

func keyOf(e Entity) entityKey { return entityKey{e.Dimension, e.ID} }

func (t *Tracker) bucket(s *state, minute int64) *Bucket {
	k := bucketKey{keyOf(s.entity), minute}
	b := t.buckets[k]
	if b == nil {
		b = &Bucket{Entity: s.entity, Minute: minute}
		t.buckets[k] = b
	}
	return b
}

func (t *Tracker) advance(s *state, now time.Time) {
	if now.Before(s.last) {
		return
	}
	if s.active == 0 {
		s.last = now
		return
	}
	// Only the retained tail is relevant after a long pause.
	if cutoff := time.Unix(now.Add(-Retention).Unix()/60*60, 0); s.last.Before(cutoff) {
		s.last = cutoff
	}
	for s.last.Before(now) {
		minute := s.last.Unix() / 60 * 60
		end := time.Unix(minute+60, 0)
		if end.After(now) {
			end = now
		}
		b := t.bucket(s, minute)
		b.ConcurrencySeconds += end.Sub(s.last).Seconds() * float64(s.active)
		b.PeakConcurrency = max(b.PeakConcurrency, s.active)
		s.last = end
	}
}

type Lease struct {
	tracker *Tracker
	keys    []entityKey
	ended   bool // guarded by tracker.mu
}

func (t *Tracker) Begin(entities ...Entity) *Lease {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	l := &Lease{tracker: t}
	seen := make(map[entityKey]bool, len(entities))
	for _, e := range entities {
		k := keyOf(e)
		if e.ID == "" || seen[k] {
			continue
		}
		seen[k] = true
		s := t.states[k]
		if s == nil {
			s = &state{entity: e, last: now, lastActivity: now}
			t.states[k] = s
		}
		t.advance(s, now)
		s.entity = e
		s.active++
		s.lastActivity = now
		b := t.bucket(s, now.Unix()/60*60)
		b.Name = e.Name
		b.Requests++
		b.PeakConcurrency = max(b.PeakConcurrency, s.active)
		pruneStarts(s, now)
		s.starts = append(s.starts, now)
		l.keys = append(l.keys, k)
	}
	return l
}

func (l *Lease) End(c Counters) {
	if l == nil {
		return
	}
	t := l.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if l.ended {
		return
	}
	l.ended = true
	now := t.now()
	for _, k := range l.keys {
		s := t.states[k]
		t.advance(s, now)
		s.active--
		s.lastActivity = now
		t.bucket(s, now.Unix()/60*60).Counters.Add(c)
	}
}

func pruneStarts(s *state, now time.Time) {
	cutoff := now.Add(-time.Minute)
	i := 0
	for i < len(s.starts) && !s.starts[i].After(cutoff) {
		i++
	}
	if i > 0 {
		copy(s.starts, s.starts[i:])
		s.starts = s.starts[:len(s.starts)-i]
	}
}

func (t *Tracker) advanceAll(now time.Time) {
	cutoff := now.Add(-Retention).Unix() / 60 * 60
	for k, b := range t.buckets {
		if b.Minute < cutoff {
			delete(t.buckets, k)
		}
	}
	for k, s := range t.states {
		t.advance(s, now)
		pruneStarts(s, now)
		if s.active == 0 && len(s.starts) == 0 && s.lastActivity.Before(now.Add(-Retention)) {
			delete(t.states, k)
		}
	}
}

func (t *Tracker) Flush(ctx context.Context) error {
	if t.store == nil {
		return nil
	}
	t.flushMu.Lock()
	defer t.flushMu.Unlock()
	t.mu.Lock()
	now := t.now()
	t.advanceAll(now)
	buckets := make([]Bucket, 0, len(t.buckets))
	for _, b := range t.buckets {
		buckets = append(buckets, *b)
	}
	t.mu.Unlock()
	if err := t.store.Save(ctx, t.runID, t.started, now, buckets); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Closed buckets cannot receive new events. Keep the open minute as an
	// overlay so a query includes traffic since the last periodic flush.
	for _, b := range buckets {
		if b.Minute < now.Unix()/60*60 {
			delete(t.buckets, bucketKey{keyOf(b.Entity), b.Minute})
		}
	}
	return nil
}

func (t *Tracker) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := t.Flush(flushCtx); err != nil && ctx.Err() == nil {
				log.Printf("traffic persistence failed: %v", err)
			}
			cancel()
		}
	}
}

func (t *Tracker) Snapshot(ctx context.Context, window time.Duration) (Snapshot, error) {
	// Refresh coverage as well as counters after a transient database outage.
	// Otherwise old persisted counts could be divided by this process's age.
	firstSeen := t.started
	if t.store != nil {
		first, err := t.store.FirstSeen(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		if !first.IsZero() && first.Before(firstSeen) {
			firstSeen = first
		}
	}
	t.mu.Lock()
	t.firstSeen = firstSeen
	now := t.now()
	t.advanceAll(now)
	since := now.Add(-window).Unix() / 60 * 60
	cutoff := now.Add(-Retention).Unix() / 60 * 60
	since = max(since, cutoff)
	effective := time.Unix(since, 0)
	if t.firstSeen.After(effective) {
		effective = t.firstSeen
	}
	seconds := max(now.Sub(effective).Seconds(), 0.001)
	result := Snapshot{Since: since, Until: now.Unix(), CollectionStartedAt: t.firstSeen.Unix(), EffectiveSince: effective.Unix(),
		Partial: t.firstSeen.After(time.Unix(since, 0)), Persistence: "memory", RetentionDays: 30, Rows: []Row{}}
	rows := map[entityKey]*Row{}
	for k, s := range t.states {
		rows[k] = &Row{Entity: s.entity, RPM: len(s.starts), CurrentConcurrency: s.active}
	}
	overlayStart := now.Unix() / 60 * 60
	for _, b := range t.buckets {
		overlayStart = min(overlayStart, b.Minute)
		if b.Minute >= since {
			k := keyOf(b.Entity)
			if rows[k] == nil {
				rows[k] = &Row{Entity: b.Entity}
			}
			rows[k].Counters.Add(b.Counters)
		}
	}
	t.mu.Unlock()
	if t.store != nil {
		result.Persistence = "database"
		saved, err := t.store.Query(ctx, since, now.Unix(), t.runID, overlayStart)
		if err != nil {
			return Snapshot{}, err
		}
		for _, b := range saved {
			k := keyOf(b.Entity)
			if rows[k] == nil {
				rows[k] = &Row{Entity: b.Entity}
			}
			rows[k].Counters.Add(b.Counters)
		}
	}
	for _, r := range rows {
		r.AverageRPM = float64(r.Requests) * 60 / seconds
		r.AverageConcurrency = r.ConcurrencySeconds / seconds
		r.CreditsConsumed = r.CreditsUpstream + r.CreditsEstimated
		if r.Completed > 0 {
			rate := float64(r.Successes) * 100 / float64(r.Completed)
			r.SuccessRate = &rate
			latency := r.LatencyMillis / float64(r.Completed)
			r.AverageLatency = &latency
		}
		if r.TTFBSamples > 0 {
			avg := r.TTFBMillis / float64(r.TTFBSamples)
			r.AverageTTFB = &avg
		}
		if r.CacheInputTokens > 0 {
			rate := float64(r.CacheReadTokens) * 100 / float64(r.CacheInputTokens)
			r.CacheRate = &rate
		}
		result.Rows = append(result.Rows, *r)
	}
	sort.Slice(result.Rows, func(i, j int) bool {
		a, b := result.Rows[i], result.Rows[j]
		if a.Dimension != b.Dimension {
			return a.Dimension < b.Dimension
		}
		return a.ID < b.ID
	})
	return result, nil
}
