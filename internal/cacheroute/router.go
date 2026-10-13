// Package cacheroute estimates prefix locality without reading upstream KV or
// retaining request text. Its estimates are hints, never authorization rules.
package cacheroute

import (
	"container/list"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/pool"
)

type Config struct {
	Disabled             bool
	Shadow               bool
	WaitTimeout          time.Duration
	MaxPending           int
	MaxPendingTotal      int
	MaxCandidates        int
	MaxCacheLoss         float64
	TTL                  time.Duration
	MaxEntries           int
	InitialPrefillTPS    float64
	Salt                 []byte
	ProtectionDisabled   bool
	FairQueueDisabled    bool
	LeaseMin, LeaseMax   time.Duration
	ReservedSlots        int
	QueueAging           time.Duration
	MaxPendingPerSession int
}

const maxPoints = 256
const blockBytes = 4096

type Point struct {
	Hash  [32]byte
	Bytes int
}
type Trace struct {
	Root       string
	Model      string
	Points     []Point
	Bytes      int
	Created    time.Time
	Generation uint64
}
type record struct {
	key        string
	uid        string
	trace      Trace
	input      int
	confidence float64
	at         time.Time
}
type performance struct {
	occupancy, prefill, density float64
	at                          time.Time
}
type Plan struct {
	UID          string
	CacheTokens  int
	InputTokens  int
	QueueSeconds float64
	CostSeconds  float64
}

type Router struct {
	mu        sync.Mutex
	cfg       Config
	records   map[string]*list.Element
	roots     map[string]map[string]*record
	lru       *list.List
	perf      map[string]performance
	nextPrune time.Time
	sequence  atomic.Uint64
}

func New(cfg Config) (*Router, error) {
	if cfg.WaitTimeout <= 0 {
		cfg.WaitTimeout = 5 * time.Second
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = 8
	}
	if cfg.MaxPendingTotal <= 0 {
		cfg.MaxPendingTotal = 64
	}
	if cfg.MaxCandidates <= 0 {
		cfg.MaxCandidates = 3
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 2048
	}
	if cfg.InitialPrefillTPS <= 0 {
		cfg.InitialPrefillTPS = 5000
	}
	if cfg.LeaseMin <= 0 {
		cfg.LeaseMin = 5 * time.Second
	}
	if cfg.LeaseMax <= 0 {
		cfg.LeaseMax = 15 * time.Second
	}
	if cfg.QueueAging <= 0 {
		cfg.QueueAging = 2 * time.Second
	}
	if cfg.MaxPendingPerSession <= 0 {
		cfg.MaxPendingPerSession = 4
	}
	// Zero uses the safe default; explicit disable is ProtectionDisabled.
	if cfg.ReservedSlots <= 0 {
		cfg.ReservedSlots = 1
	}
	if cfg.LeaseMax < cfg.LeaseMin {
		return nil, fmt.Errorf("lease_max must be >= lease_min")
	}
	if cfg.MaxCacheLoss < 0 || cfg.MaxCacheLoss > 1 || math.IsNaN(cfg.MaxCacheLoss) {
		return nil, fmt.Errorf("max cache loss must be between 0 and 1")
	}
	cfg.Salt = append([]byte(nil), cfg.Salt...)
	if len(cfg.Salt) == 0 {
		cfg.Salt = make([]byte, 32)
		if _, err := rand.Read(cfg.Salt); err != nil {
			return nil, err
		}
	}
	return &Router{cfg: cfg, records: map[string]*list.Element{}, roots: map[string]map[string]*record{}, lru: list.New(), perf: map[string]performance{}}, nil
}

func (r *Router) Config() Config { c := r.cfg; c.Salt = nil; return c }
func (r *Router) Hash(value string) string {
	if value == "" {
		return ""
	}
	m := hmac.New(sha256.New, r.cfg.Salt)
	m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil))
}

// ScopedKey hashes the entire identity before session's normalization. Model,
// key identity and authorization namespace cannot collide with client IDs.
func ScopedKey(namespace, model, raw string) string {
	b, _ := json.Marshal([]string{namespace, model, raw})
	v := sha256.Sum256(b)
	return hex.EncodeToString(v[:])
}

// Build consumes the already normalized outbound document. It never hashes
// the whole JSON body as an affinity key: appending history preserves Root.
func (r *Router) Build(namespace, model string, doc map[string]any) Trace {
	t := Trace{Model: model, Created: time.Now(), Generation: r.sequence.Add(1)}
	messages, _ := doc["messages"].([]any)
	first := -1
	for i, raw := range messages {
		if m, ok := raw.(map[string]any); ok && m["role"] == "user" {
			first = i
			break
		}
	}
	if first < 0 {
		return t
	}
	settings := map[string]any{}
	for k, v := range doc {
		switch k {
		case "messages", "stream", "metadata", "user", "prompt_cache_key", "prompt_cache_retention", "conversation", "conversation_id",
			"max_tokens", "max_completion_tokens", "temperature", "top_p", "seed", "stream_options", "n":
			continue
		}
		settings[k] = v
	}
	header, _ := json.Marshal([]any{namespace, model, settings})
	mac := hmac.New(sha256.New, r.cfg.Salt)
	stride := blockBytes
	var rootPoint Point
	addPoint := func() {
		if len(t.Points) > 0 && t.Points[len(t.Points)-1].Bytes == t.Bytes {
			return
		}
		// Keep the entire history represented within a fixed memory budget.
		// Powers-of-two sampling lets traces of different lengths still share
		// checkpoint positions; retain root and final boundaries separately.
		if len(t.Points) >= maxPoints-2 {
			stride *= 2
			kept := t.Points[:0]
			for _, p := range t.Points {
				if p.Bytes%stride == 0 {
					kept = append(kept, p)
				}
			}
			t.Points = kept
		}
		var p Point
		p.Bytes = t.Bytes
		copy(p.Hash[:], mac.Sum(nil))
		t.Points = append(t.Points, p)
	}
	feed := func(data []byte) {
		for len(data) > 0 {
			n := blockBytes - t.Bytes%blockBytes
			if n > len(data) {
				n = len(data)
			}
			mac.Write(data[:n])
			t.Bytes += n
			data = data[n:]
			if t.Bytes%stride == 0 {
				addPoint()
			}
		}
	}
	feed(header)
	feed([]byte{0})
	for i, raw := range messages {
		b, err := json.Marshal(raw)
		if err != nil {
			return Trace{}
		}
		feed(b)
		feed([]byte{0})
		addPoint()
		if i == first {
			t.Root = hex.EncodeToString(mac.Sum(nil))
			rootPoint = t.Points[len(t.Points)-1]
		}
	}
	rootPresent := false
	for _, p := range t.Points {
		if p.Bytes == rootPoint.Bytes {
			rootPresent = true
			break
		}
	}
	if !rootPresent {
		t.Points = append(t.Points, rootPoint)
		sort.Slice(t.Points, func(i, j int) bool { return t.Points[i].Bytes < t.Points[j].Bytes })
	}
	return t
}

func commonBytes(a, b Trace) int {
	points := make(map[[32]byte]int, len(a.Points))
	for _, p := range a.Points {
		points[p.Hash] = p.Bytes
	}
	n := 0
	for _, p := range b.Points {
		if points[p.Hash] == p.Bytes && p.Bytes > n {
			n = p.Bytes
		}
	}
	return n
}

func (r *Router) remove(el *list.Element) {
	rec := el.Value.(*record)
	delete(r.records, rec.key)
	delete(r.roots[rec.trace.Root], rec.uid)
	if len(r.roots[rec.trace.Root]) == 0 {
		delete(r.roots, rec.trace.Root)
	}
	r.lru.Remove(el)
}

func (r *Router) prune(now time.Time) {
	if now.Before(r.nextPrune) {
		return
	}
	interval := r.cfg.TTL / 4
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	r.nextPrune = now.Add(interval)
	for el := r.lru.Back(); el != nil; {
		prev := el.Prev()
		if now.Sub(el.Value.(*record).at) > r.cfg.TTL {
			r.remove(el)
		}
		el = prev
	}
	for k, p := range r.perf {
		if now.Sub(p.at) > r.cfg.TTL {
			delete(r.perf, k)
		}
	}
}

// Plans selects a small shortlist with comparable cache coverage, then ranks
// estimated wait + uncached prefill. Near ties keep the existing primary.
func (r *Router) Plans(primary string, t Trace, states []pool.RoutingState, tried map[string]bool) []Plan {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.prune(now)
	var plans []Plan
	best := 0
	density := 1.0 / 3
	bestMatch := 0
	var newest uint64
	for _, s := range states {
		if !s.Healthy || tried[s.UID] {
			continue
		}
		rec := r.roots[t.Root][s.UID]
		if rec != nil && now.Sub(rec.at) > r.cfg.TTL {
			rec = nil
		}
		if s.UID != primary && rec == nil {
			continue
		}
		cached := 0
		if rec != nil {
			matched := commonBytes(rec.trace, t)
			cached = int(float64(rec.input) * float64(matched) / float64(rec.trace.Bytes) * rec.confidence)
			if matched > bestMatch || (matched == bestMatch && rec.trace.Generation > newest) {
				bestMatch = matched
				newest = rec.trace.Generation
				density = float64(rec.input) / float64(rec.trace.Bytes)
			}
		}
		p := r.perf[s.UID+"\x00"+t.Model]
		if now.Sub(p.at) > r.cfg.TTL {
			p = performance{}
		}
		if bestMatch == 0 && s.UID == primary && p.density > 0 {
			density = p.density
		}
		wait := 0.0
		if s.Waiting > 0 || (s.Limit > 0 && s.InFlight >= s.Limit) {
			occupancy := p.occupancy
			if occupancy <= 0 {
				occupancy = r.cfg.WaitTimeout.Seconds()
			}
			limit := s.Limit
			if limit <= 0 {
				limit = 1
			}
			waves := float64(s.Waiting+max(0, s.InFlight-limit)+1) / float64(limit)
			wait = occupancy * math.Max(1.0/float64(limit), waves)
		}
		plans = append(plans, Plan{UID: s.UID, CacheTokens: cached, QueueSeconds: wait})
		if cached > best {
			best = cached
		}
	}
	// All candidates process the same request: do not compare costs computed
	// from different input estimates. The closest recorded prefix supplies a
	// common density; throughput and queue estimates remain account-specific.
	input := max(best, int(float64(t.Bytes)*density))
	for i := range plans {
		p := &plans[i]
		perf := r.perf[p.UID+"\x00"+t.Model]
		v := perf.prefill
		if v <= 0 || now.Sub(perf.at) > r.cfg.TTL {
			v = r.cfg.InitialPrefillTPS
		}
		p.InputTokens = input
		p.CostSeconds = p.QueueSeconds + float64(max(0, input-p.CacheTokens))/v
	}
	floor := float64(best) * (1 - r.cfg.MaxCacheLoss)
	kept := plans[:0]
	for _, p := range plans {
		if float64(p.CacheTokens) >= floor {
			kept = append(kept, p)
		}
	}
	plans = kept
	sort.Slice(plans, func(i, j int) bool {
		a, b := plans[i], plans[j]
		if a.CostSeconds != b.CostSeconds {
			return a.CostSeconds < b.CostSeconds
		}
		return a.UID < b.UID
	})
	if len(plans) > 1 && plans[0].UID != primary {
		for i, p := range plans {
			if p.UID == primary && p.CostSeconds-plans[0].CostSeconds < .1 {
				copy(plans[1:i+1], plans[:i])
				plans[0] = p
				break
			}
		}
	}
	if len(plans) > r.cfg.MaxCandidates {
		plans = plans[:r.cfg.MaxCandidates]
	}
	return plans
}

// Observe only records successful requests with measured input usage. A late
// completion cannot replace a newer prefix record. Actual cache misses reduce
// confidence; newly processed input remains a best-effort cache hint.
func (r *Router) Observe(uid string, t Trace, input, cache, predicted int, cacheKnown bool, ttfb, occupancy time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.prune(now)
	key := uid + "\x00" + t.Model
	perf := r.perf[key]
	ewma := func(old, new float64) float64 {
		if old <= 0 {
			return new
		}
		return old*.8 + new*.2
	}
	if occupancy > 0 {
		perf.occupancy = ewma(perf.occupancy, occupancy.Seconds())
	}
	if input > 0 && t.Bytes > 0 {
		perf.density = ewma(perf.density, float64(input)/float64(t.Bytes))
	}
	if cacheKnown && input-cache >= 512 && ttfb > 0 {
		rate := float64(input-cache) / ttfb.Seconds()
		perf.prefill = ewma(perf.prefill, math.Max(100, math.Min(rate, 500000)))
	}
	perf.at = now
	if _, ok := r.perf[key]; !ok && len(r.perf) >= r.cfg.MaxEntries {
		oldest := ""
		at := now
		for k, p := range r.perf {
			if oldest == "" || p.at.Before(at) {
				oldest = k
				at = p.at
			}
		}
		delete(r.perf, oldest)
	}
	r.perf[key] = perf
	if input <= 0 || t.Root == "" || len(t.Points) == 0 {
		return
	}
	key = t.Root + "\x00" + uid
	confidence := 1.0
	if el := r.records[key]; el != nil {
		old := el.Value.(*record)
		if t.Generation < old.trace.Generation {
			return
		}
		confidence = old.confidence
		r.remove(el)
	}
	if cacheKnown && predicted > 0 && cache < predicted*7/10 {
		confidence = math.Max(.2, confidence*.5)
	} else {
		confidence = math.Min(1, confidence+.1)
	}
	for len(r.records) >= r.cfg.MaxEntries {
		r.remove(r.lru.Back())
	}
	t.Points = append([]Point(nil), t.Points...)
	rec := &record{key, uid, t, input, confidence, now}
	r.records[key] = r.lru.PushFront(rec)
	if r.roots[t.Root] == nil {
		r.roots[t.Root] = map[string]*record{}
	}
	r.roots[t.Root][uid] = rec
}

func (r *Router) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune(time.Now())
	return len(r.records)
}
