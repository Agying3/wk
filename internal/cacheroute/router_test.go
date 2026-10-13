package cacheroute

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

func testRouter(t *testing.T, cfg Config) *Router {
	t.Helper()
	cfg.Salt = []byte("offline-test-salt")
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func prompt(task string) map[string]any {
	return map[string]any{"model": "m", "messages": []any{map[string]any{"role": "system", "content": "same instructions"}, map[string]any{"role": "user", "content": task}}}
}
func appendMessage(doc map[string]any, role, content string) {
	doc["messages"] = append(doc["messages"].([]any), map[string]any{"role": role, "content": content})
}
func state(uid string, full bool) pool.RoutingState {
	n := 0
	if full {
		n = 1
	}
	return pool.RoutingState{UID: uid, Healthy: true, Limit: 1, InFlight: n}
}

func TestPrefixSurvivesAppendAndRejectsDifferentTasksOrSettings(t *testing.T) {
	r := testRouter(t, Config{})
	doc := prompt("task-a")
	a := r.Build("group-a", "m", doc)
	appendMessage(doc, "assistant", strings.Repeat("history", 3000))
	b := r.Build("group-a", "m", doc)
	if a.Root != b.Root || commonBytes(a, b) != a.Bytes {
		t.Fatal("append lost the earlier prefix")
	}
	for _, other := range []Trace{r.Build("group-b", "m", doc), r.Build("group-a", "other", doc), r.Build("group-a", "m", prompt("task-b"))} {
		if other.Root == a.Root {
			t.Fatal("scope/model/task collision")
		}
	}
	doc["tools"] = []any{map[string]any{"name": "new-tool"}}
	if r.Build("group-a", "m", doc).Root == a.Root {
		t.Fatal("tool change reused prefix")
	}
}

func TestPrefixPlansFindLongerHistoryAndRespectScopeAndModelHealth(t *testing.T) {
	r := testRouter(t, Config{MaxCacheLoss: .1})
	doc := prompt("task")
	appendMessage(doc, "assistant", strings.Repeat("a", 10000))
	old := r.Build("g", "m", doc)
	r.Observe("A", old, 10000, 0, 0, true, 0, time.Second)
	appendMessage(doc, "user", strings.Repeat("b", 10000))
	recent := r.Build("g", "m", doc)
	r.Observe("B", recent, 20000, 0, 0, true, 0, time.Second)
	appendMessage(doc, "assistant", "answer")
	current := r.Build("g", "m", doc)
	plans := r.Plans("A", current, []pool.RoutingState{state("A", false), state("B", false)}, nil)
	if len(plans) == 0 || plans[0].UID != "B" || plans[0].CacheTokens <= 10000 {
		t.Fatalf("old account preferred: %+v", plans)
	}
	plans = r.Plans("A", current, []pool.RoutingState{state("A", false)}, nil)
	if len(plans) != 1 || plans[0].UID != "A" {
		t.Fatal("account outside authorized states was used")
	}
	bad := state("B", false)
	bad.Healthy = false
	plans = r.Plans("A", current, []pool.RoutingState{state("A", false), bad}, nil)
	if len(plans) != 1 || plans[0].UID != "A" {
		t.Fatal("model-ineligible account was used")
	}
}

func TestNearlyWarmBackupBeatsLongWait(t *testing.T) {
	r := testRouter(t, Config{MaxCacheLoss: .1})
	trace := r.Build("g", "m", prompt("task"))
	r.Observe("A", trace, 20000, 0, 0, true, 0, 6*time.Second)
	r.Observe("B", trace, 20000, 0, 0, true, 0, time.Second)
	r.mu.Lock()
	r.roots[trace.Root]["B"].confidence = .95
	r.mu.Unlock()
	plans := r.Plans("A", trace, []pool.RoutingState{state("A", true), state("B", false)}, nil)
	if len(plans) == 0 || plans[0].UID != "B" {
		t.Fatalf("did not borrow warm backup: %+v", plans)
	}
}

func TestLongHistoryCheckpointsCoverBeyondFirstMegabyte(t *testing.T) {
	r := testRouter(t, Config{MaxCacheLoss: .1})
	doc := prompt("task")
	appendMessage(doc, "assistant", strings.Repeat("old", 400000))
	old := r.Build("g", "m", doc)
	r.Observe("A", old, 400000, 0, 0, false, 0, time.Second)
	appendMessage(doc, "user", strings.Repeat("new", 400000))
	recent := r.Build("g", "m", doc)
	r.Observe("B", recent, 800000, 0, 0, false, 0, time.Second)
	appendMessage(doc, "assistant", "answer")
	current := r.Build("g", "m", doc)
	if commonBytes(recent, current) < recent.Bytes-32768 {
		t.Fatal("long prefix was truncated to early checkpoints")
	}
	for _, trace := range []Trace{old, recent, current} {
		if len(trace.Points) > maxPoints {
			t.Fatal("checkpoint bound exceeded")
		}
	}
	plans := r.Plans("A", current, []pool.RoutingState{state("A", false), state("B", false)}, nil)
	if len(plans) == 0 || plans[0].UID != "B" || plans[0].CacheTokens < 750000 {
		t.Fatalf("longer cached history not found: %+v", plans)
	}
}

func TestActualMissCorrectsPredictionAndLateObservationCannotReplaceNewer(t *testing.T) {
	r := testRouter(t, Config{})
	doc := prompt("task")
	old := r.Build("g", "m", doc)
	r.Observe("A", old, 10000, 0, 0, true, 0, time.Second)
	appendMessage(doc, "assistant", "next")
	newer := r.Build("g", "m", doc)
	plans := r.Plans("A", newer, []pool.RoutingState{state("A", false)}, nil)
	r.Observe("A", newer, 20000, 0, plans[0].CacheTokens, true, 0, time.Second)
	r.Observe("A", old, 10000, 10000, 0, true, 0, time.Second)
	r.mu.Lock()
	rec := r.roots[newer.Root]["A"]
	r.mu.Unlock()
	if rec.trace.Bytes != newer.Bytes || rec.confidence >= 1 {
		t.Fatalf("bad correction/late write: %+v", rec)
	}
}

func TestIndexTTLBoundsAndNoRequestTextRetention(t *testing.T) {
	r := testRouter(t, Config{MaxEntries: 2, TTL: 15 * time.Millisecond})
	for i := 0; i < 3; i++ {
		trace := r.Build("g", "m", prompt(fmt.Sprintf("private-task-%d", i)))
		r.Observe("A", trace, 1000, 0, 0, false, 0, time.Second)
	}
	if r.Size() != 2 {
		t.Fatalf("size=%d", r.Size())
	}
	trace := r.Build("g", "m", prompt(strings.Repeat("private", 300000)))
	if len(trace.Points) > maxPoints {
		t.Fatal("unbounded fingerprints")
	}
	raw, _ := json.Marshal(trace)
	if strings.Contains(string(raw), "private") {
		t.Fatal("text retained in trace")
	}
	time.Sleep(20 * time.Millisecond)
	if r.Size() != 0 {
		t.Fatal("expired records retained")
	}
}

func TestConcurrentPrefixObservationAndSelection(t *testing.T) {
	r := testRouter(t, Config{MaxEntries: 8})
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			trace := r.Build("g", "m", prompt(fmt.Sprintf("task-%d", i%6)))
			uid := fmt.Sprintf("u%d", i%4)
			for n := 0; n < 20; n++ {
				r.Observe(uid, trace, 1000, 0, 0, false, 0, time.Second)
				r.Plans(uid, trace, []pool.RoutingState{state(uid, false)}, nil)
			}
		}(i)
	}
	wg.Wait()
	if r.Size() > 8 {
		t.Fatal("concurrent index exceeded bound")
	}
}

func TestPlansCompareOneRequestInputEstimate(t *testing.T) {
	r := testRouter(t, Config{MaxCacheLoss: 1})
	trace := r.Build("g", "m", prompt("task"))
	r.Observe("A", trace, 20000, 0, 0, false, 0, time.Second)
	r.Observe("B", trace, 19000, 0, 0, false, 0, time.Second)
	plans := r.Plans("A", trace, []pool.RoutingState{state("A", false), state("B", false)}, nil)
	if len(plans) != 2 || plans[0].InputTokens != plans[1].InputTokens {
		t.Fatalf("incomparable input estimates: %+v", plans)
	}
}

func BenchmarkPrefixBuildAndPlans(b *testing.B) {
	r, _ := New(Config{Salt: []byte("bench"), MaxCacheLoss: .1})
	doc := prompt(strings.Repeat("context", 30000))
	trace := r.Build("g", "m", doc)
	r.Observe("A", trace, 100000, 0, 0, false, 0, time.Second)
	states := []pool.RoutingState{state("A", false)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		t := r.Build("g", "m", doc)
		r.Plans("A", t, states, nil)
	}
}
