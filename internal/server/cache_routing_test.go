package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/cacheroute"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

func TestCacheRouteOtherSessionsUseSlotsWithoutDisplacingWarmPrimary(t *testing.T) {
	p := routingPool()
	p.SetMaxInFlight(3)
	sess := routingSession(p)
	sess.Bind(testScopedChatKey("c", "glm-5.2"), "A")
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-A" {
			n := calls.Add(1)
			if n == 2 || n == 3 {
				started <- struct{}{}
				<-release
			}
		}
		return 200, routingSSE, true
	})
	h := newTestHandler(t, Config{Pool: p, Upstream: up, Session: sess})
	serve := func(body string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		return rec.Code
	}
	if serve(routingBody) != 200 {
		t.Fatal("warmup failed")
	}
	primary, _ := sess.LookupLease(testScopedChatKey("c", "glm-5.2"))
	finished := make(chan int, 2)
	for _, id := range []string{"other-1", "other-2"} {
		sess.Bind(testScopedChatKey(id, "glm-5.2"), "A")
		body := fmt.Sprintf(`{"model":"glm-5.2","messages":[{"role":"user","content":%q}],"metadata":{"conversation_id":%q}}`, id, id)
		go func() { finished <- serve(body) }()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("foreign session did not start")
		}
	}
	// Two other conversations are running on A. A fresh conversation must go
	// to B, while c can acquire A's third slot without changing its lease.
	fresh := `{"model":"glm-5.2","messages":[{"role":"user","content":"new-task"}],"metadata":{"conversation_id":"fresh"}}`
	if serve(fresh) != 200 {
		t.Fatal("new request failed")
	}
	freshLease, _ := sess.LookupLease(testScopedChatKey("fresh", "glm-5.2"))
	if freshLease.UID != "B" {
		t.Fatal("new request crowded active account", freshLease.UID)
	}
	if serve(routingBody) != 200 {
		t.Fatal("returning request failed")
	}
	after, _ := sess.LookupLease(testScopedChatKey("c", "glm-5.2"))
	if after != primary {
		t.Fatal("foreign requests displaced primary")
	}
	if calls.Load() != 4 {
		t.Fatal("warm session did not return to A")
	}
	releaseOnce.Do(func() { close(release) })
	for i := 0; i < 2; i++ {
		select {
		case code := <-finished:
			if code != 200 {
				t.Fatal("foreign request failed", code)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("foreign request did not finish")
		}
	}
	for _, state := range p.RoutingStates("glm-5.2", nil) {
		if state.InFlight != 0 || state.Waiting != 0 {
			t.Fatal("route leaked capacity")
		}
	}
}

func TestCacheRouteWaitsWhenDifferentSessionsFillAllSlots(t *testing.T) {
	p := routingPool()
	p.SetMaxInFlight(3)
	sess := routingSession(p)
	sess.Bind(testScopedChatKey("c", "glm-5.2"), "A")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at-A" {
			t.Error("returning conversation switched account")
		}
		return 200, routingSSE, true
	})
	h := newTestHandler(t, Config{Pool: p, Upstream: up, Session: sess})
	// Foreign sessions are authorized only for A, so all three slots can be
	// borrowed. The protected conversation must retain A and queue.
	requests := []pool.RouteRequest{{Tenant: "foreign", Session: "s2", Prefix: "p2"}, {Tenant: "foreign", Session: "s3", Prefix: "p3"}, {Tenant: "foreign", Session: "s4", Prefix: "p4"}}
	for _, r := range requests {
		if p.PickAndAcquireByUIDForRoute("A", "glm-5.2", r, nil, map[string]bool{"A": true}) == nil {
			t.Fatal("foreign acquire failed")
		}
	}
	defer p.ReleaseRoute("A", requests[1])
	defer p.ReleaseRoute("A", requests[2])
	result := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(routingBody)))
		result <- rec.Code
	}()
	awaitRouteQueue(t, p, "A", 1)
	p.ReleaseRoute("A", requests[0])
	select {
	case code := <-result:
		if code != 200 {
			t.Fatal(code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("returning session remained blocked")
	}
	lease, _ := sess.LookupLease(testScopedChatKey("c", "glm-5.2"))
	if lease.UID != "A" {
		t.Fatal("full foreign account changed binding")
	}
}

func testScopedChatKey(raw, model string) string {
	return cacheroute.ScopedKey(`"admin"/""/""`, model, raw)
}
func routingPool() *pool.Pool {
	return testPoolWith(&auth.Auth{UID: "A", AccessToken: "at-A", ExpiresAt: 9999999999}, &auth.Auth{UID: "B", AccessToken: "at-B", ExpiresAt: 9999999999})
}
func routingSession(p *pool.Pool) *session.Router {
	return session.New(session.Config{Available: p.AvailableUIDs, Healthy: p.HealthyUIDs, Salt: []byte("offline-test-salt")})
}
func awaitRouteQueue(t *testing.T, p *pool.Pool, uid string, n int) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		for _, s := range p.RoutingStates("glm-5.2", nil) {
			if s.UID == uid && s.Waiting == n {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s queue did not reach %d", uid, n)
}

const routingBody = `{"model":"glm-5.2","messages":[{"role":"user","content":"task"}],"metadata":{"conversation_id":"c"}}`
const routingSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":20,\"prompt_cache_hit_tokens\":9000}}\n\ndata: [DONE]\n\n"

func TestCacheRouteWaitsForPrimaryAndReleasesSlot(t *testing.T) {
	p := routingPool()
	p.SetMaxInFlight(1)
	p.Acquire("A")
	sess := routingSession(p)
	sess.Bind(testScopedChatKey("c", "glm-5.2"), "A")
	called := make(chan string, 1)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { called <- authz; return 200, routingSSE, true })
	h := newTestHandler(t, Config{Pool: p, Upstream: up, Session: sess, CacheRouting: cacheroute.Config{WaitTimeout: time.Second, MaxCacheLoss: .1}})
	result := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(routingBody)))
		result <- rec.Code
	}()
	awaitRouteQueue(t, p, "A", 1)
	select {
	case <-called:
		t.Fatal("sent upstream before primary was free")
	default:
	}
	p.Release("A")
	if code := <-result; code != 200 {
		t.Fatal(code)
	}
	if authz := <-called; authz != "Bearer at-A" {
		t.Fatal("did not reuse primary")
	}
	for _, s := range p.RoutingStates("glm-5.2", nil) {
		if s.InFlight != 0 || s.Waiting != 0 {
			t.Fatalf("leaked capacity: %+v", s)
		}
	}
}

func TestCacheRouteCancellationSendsNoUpstreamRequest(t *testing.T) {
	p := routingPool()
	p.SetMaxInFlight(1)
	p.Acquire("A")
	defer p.Release("A")
	sess := routingSession(p)
	sess.Bind(testScopedChatKey("c", "glm-5.2"), "A")
	var calls atomic.Int64
	up := newFakeUpstream(t, func(string) (int, string, bool) { calls.Add(1); return 200, routingSSE, true })
	h := newTestHandler(t, Config{Pool: p, Upstream: up, Session: sess})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(routingBody)).WithContext(ctx)
		h.ServeHTTP(rec, req)
		result <- rec.Code
	}()
	awaitRouteQueue(t, p, "A", 1)
	cancel()
	if code := <-result; code != 408 {
		t.Fatal(code)
	}
	if calls.Load() != 0 {
		t.Fatal("cancelled request reached upstream")
	}
	awaitRouteQueue(t, p, "A", 0)
}

func TestCacheRouteLateSuccessfulRequestCannotUndoMigration(t *testing.T) {
	p := routingPool()
	sess := routingSession(p)
	sess.Bind(testScopedChatKey("c", "glm-5.2"), "A")
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var aCalls atomic.Int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-A" {
			if aCalls.Add(1) == 1 {
				close(started)
				<-release
				return 200, routingSSE, true
			}
			return 402, `{"msg":"余额不足"}`, false
		}
		return 200, routingSSE, true
	})
	h := newTestHandler(t, Config{Pool: p, Upstream: up, Session: sess})
	first := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(routingBody)))
		first <- rec.Code
	}()
	<-started
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(routingBody)))
	if rec.Code != 200 {
		t.Fatalf("migration request: %d %s", rec.Code, rec.Body)
	}
	once.Do(func() { close(release) })
	if code := <-first; code != 200 {
		t.Fatal(code)
	}
	e, ok := sess.ResolveLeaseWithCandidates(testScopedChatKey("c", "glm-5.2"), nil, "", []string{"A", "B"}, []string{"A", "B"})
	if !ok || e.UID != "B" {
		t.Fatalf("late completion restored old primary: %+v", e)
	}
}

func TestCacheRouteMissingIDTracksHistoryAndSeparatesTasks(t *testing.T) {
	p := routingPool()
	sess := routingSession(p)
	var seen []string
	var mu sync.Mutex
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		seen = append(seen, authz)
		mu.Unlock()
		return 200, routingSSE, true
	})
	h := newTestHandler(t, Config{Pool: p, Upstream: up, Session: sess})
	doc := map[string]any{"model": "glm-5.2", "metadata": map[string]any{"user_id": "shared-user"}, "messages": []any{map[string]any{"role": "user", "content": "task-a"}}}
	for i := 0; i < 4; i++ {
		body, _ := json.Marshal(doc)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body))))
		if rec.Code != 200 {
			t.Fatal(rec.Code)
		}
		doc["messages"] = append(doc["messages"].([]any), map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "user", "content": "continue"})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","metadata":{"user_id":"shared-user"},"messages":[{"role":"user","content":"task-b"}]}`)))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if len(seen) != 5 {
		t.Fatal(seen)
	}
	for _, uid := range seen[:4] {
		if uid != seen[0] {
			t.Fatal("appending history changed account")
		}
	}
	if seen[4] == seen[0] || sess.Count() != 2 {
		t.Fatal("user_id merged independent tasks")
	}
}

func TestCacheRouteAuthorizationAndModelScope(t *testing.T) {
	p := routingPool()
	sess := routingSession(p)
	up := upstream.New()
	h := newTestHandler(t, Config{Pool: p, Upstream: up, Session: sess})
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(context.WithValue(context.Background(), scopeKey{}, requestScope{kind: "group", group: "g", keyID: "k"}))
	doc := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "task"}}}
	_, _ = up.PrepareChatBody(doc)
	trace := h.cacheRouter.Build(routeNamespace(req), "m", doc)
	h.cacheRouter.Observe("A", trace, 10000, 9000, 0, true, 0, time.Second)
	key := cacheroute.ScopedKey(routeNamespace(req), "m", "c")
	sess.Bind(key, "A")
	route := h.beginChatRoute(req, "c", "conversation_id", "m", doc, map[string]bool{"B": true})
	acct, err := route.acquire(req.Context(), nil, map[string]bool{"B": true})
	if err != nil || acct == nil || acct.UID != "B" {
		t.Fatalf("scope escaped: %v %v", acct, err)
	}
	p.Release(acct.UID)
	p.CooldownModel("A", "limited", time.Minute, "test")
	sess.Bind(cacheroute.ScopedKey(routeNamespace(req), "limited", "c"), "A")
	route = h.beginChatRoute(req, "c", "conversation_id", "limited", doc, nil)
	acct, err = route.acquire(req.Context(), nil, nil)
	if err != nil || acct == nil || acct.UID != "B" {
		t.Fatal("model cooldown ignored")
	}
	p.Release(acct.UID)
}

func TestCacheRouteCarriesLegacyExplicitBindingIntoNamespace(t *testing.T) {
	p := routingPool()
	sess := routingSession(p)
	sess.Bind("legacy-c", "A")
	h := newTestHandler(t, Config{Pool: p, Session: sess})
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(context.WithValue(context.Background(), scopeKey{}, requestScope{kind: "admin"}))
	doc := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "task"}}}
	route := h.beginChatRoute(req, "legacy-c", "conversation_id", "m", doc, nil)
	if route.lease.UID != "A" {
		t.Fatal("upgrade discarded existing warm account")
	}
	if _, ok := sess.LookupLease(cacheroute.ScopedKey(routeNamespace(req), "m", "legacy-c")); !ok {
		t.Fatal("namespaced binding missing")
	}
}

func TestStatsPartialUsageKeepsCacheAndHonorsExplicitZero(t *testing.T) {
	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	for _, line := range []string{
		`data: {"usage":{"input_tokens":1000,"prompt_cache_hit_tokens":800}}`,
		`data: {"usage":{"output_tokens":20}}`,
		`data: {"usage":{"credit":0.1}}`,
		`data: {"usage":{}}`,
	} {
		s.parseSSELine(line)
	}
	input, output, total, cache, _, _ := s.UsageStats()
	if input != 1000 || output != 20 || total != 1020 || cache != 800 || !s.CacheUsageKnown() {
		t.Fatalf("partial snapshot lost: %d %d %d %d", input, output, total, cache)
	}
	s.parseSSELine(`data: {"usage":{"input_tokens_details":{"cached_tokens":0}}}`)
	_, _, _, cache, _, _ = s.UsageStats()
	if cache != 0 {
		t.Fatal("explicit zero was ignored")
	}
}

func TestResponseUsageBackfillsEmptyDetailsWithoutChangingExplicitZero(t *testing.T) {
	for _, tc := range []struct {
		details map[string]any
		want    int
	}{
		{map[string]any{}, 800}, {map[string]any{"cached_tokens": 0}, 0},
	} {
		raw := map[string]any{"prompt_tokens": 1000, "completion_tokens": 20, "prompt_cache_hit_tokens": 800, "input_tokens_details": tc.details}
		out := responseUsage(raw).(map[string]any)
		got, _ := numberInt(out["input_tokens_details"].(map[string]any)["cached_tokens"])
		if got != tc.want {
			t.Fatalf("cache=%d want %d", got, tc.want)
		}
		if out["total_tokens"] != 1020 {
			t.Fatal("total not reconstructed")
		}
	}
}

func TestCacheRouteWarmBorrowAndShadow(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		t.Run(fmt.Sprint(shadow), func(t *testing.T) {
			p := routingPool()
			if shadow {
				p.Add(&auth.Auth{UID: "C", AccessToken: "at-C", ExpiresAt: 9999999999})
				p.SetCredits("C", 100000)
				p.SetRandomSource(func(int64) int64 { return 0 })
			}
			p.SetMaxInFlight(1)
			p.Acquire("A")
			defer p.Release("A")
			sess := routingSession(p)
			key := testScopedChatKey("c", "glm-5.2")
			sess.Bind(key, "A")
			h := newTestHandler(t, Config{Pool: p, Upstream: upstream.New(), Session: sess, CacheRouting: cacheroute.Config{WaitTimeout: time.Second, MaxCacheLoss: .1, Shadow: shadow}})
			req := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(context.WithValue(context.Background(), scopeKey{}, requestScope{kind: "admin"}))
			var doc map[string]any
			json.Unmarshal([]byte(routingBody), &doc)
			h.cfg.Upstream.PrepareChatBody(doc)
			trace := h.cacheRouter.Build(routeNamespace(req), "glm-5.2", doc)
			h.cacheRouter.Observe("A", trace, 10000, 9000, 0, true, 0, 10*time.Second)
			h.cacheRouter.Observe("B", trace, 10000, 9000, 0, true, 0, time.Second)
			route := h.beginChatRoute(req, "c", "metadata.conversation_id", "glm-5.2", doc, nil)
			acct, err := route.acquire(req.Context(), nil, nil)
			want := "B"
			if shadow {
				want = "C"
			}
			if err != nil || acct == nil || acct.UID != want {
				t.Fatalf("warm backup=%v %v", acct, err)
			}
			p.Release(acct.UID)
			if route.lease.UID != "A" || route.wait > 0 {
				t.Fatal("borrowing changed primary or waited unnecessarily")
			}
			if shadow && route.shadowUID != "B" {
				t.Fatal("shadow suggestion missing")
			}
		})
	}
}

type routingTransport func(*http.Request) (*http.Response, error)

func (f routingTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCacheRouteRetriesUseIdenticalNormalizedBody(t *testing.T) {
	p := routingPool()
	sess := routingSession(p)
	sess.Bind(testScopedChatKey("c", "glm-5.2"), "A")
	up := upstream.New()
	var bodies []string
	up.HTTP.Transport = routingTransport(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		bodies = append(bodies, string(raw))
		code := 200
		body := routingSSE
		if len(bodies) == 1 {
			code = 402
			body = `{"msg":"余额不足"}`
		}
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	h := newTestHandler(t, Config{Pool: p, Upstream: up, Session: sess})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(routingBody)))
	if rec.Code != 200 || len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("body changed between attempts: %d %v", rec.Code, bodies)
	}
	if strings.Contains(bodies[0], "conversation_id") || !strings.Contains(bodies[0], `"stream":true`) {
		t.Fatal("body bypassed normalization")
	}
}
