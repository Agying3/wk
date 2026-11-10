package handoff

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func gatewayFor(t *testing.T, target string) *Gateway {
	t.Helper()
	g, err := New(Config{Target: target, Release: "a", MaxPending: 2, MaxActive: 4, MaxBody: 100000, MaxQueuedBytes: 300000, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func control(t *testing.T, g *Gateway, path string, want int) {
	t.Helper()
	w := httptest.NewRecorder()
	g.Control().ServeHTTP(w, httptest.NewRequest("POST", path, nil))
	if w.Code != want {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
}
func TestStreamDrainsAndQueuedPOSTRunsOnlyOnNewRelease(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: old\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-finish
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer a.Close()
	g := gatewayFor(t, a.URL)
	ingress := httptest.NewServer(g)
	defer ingress.Close()
	oldDone := make(chan string, 1)
	go func() {
		r, e := http.Get(ingress.URL)
		if e != nil {
			oldDone <- e.Error()
			return
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		oldDone <- string(b)
	}()
	<-started
	control(t, g, "/hold?expected=a", 204)
	control(t, g, "/switch?expected=a&next=b&target="+a.URL, 409)
	newCalls := make(chan string, 2)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		newCalls <- string(raw)
		io.WriteString(w, "new")
	}))
	defer b.Close()
	newDone := make(chan string, 1)
	go func() {
		r, e := http.Post(ingress.URL, "application/json", strings.NewReader(`{"input":"one"}`))
		if e != nil {
			newDone <- e.Error()
			return
		}
		defer r.Body.Close()
		v, _ := io.ReadAll(r.Body)
		newDone <- string(v)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		g.mu.Lock()
		n := len(g.queue)
		g.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request not queued")
		}
		time.Sleep(time.Millisecond)
	}
	close(finish)
	if v := <-oldDone; !strings.Contains(v, "[DONE]") {
		t.Fatal(v)
	}
	control(t, g, "/switch?expected=a&next=b&target="+b.URL, 204)
	control(t, g, "/resume?expected=a", 409)
	control(t, g, "/resume?expected=b", 204)
	if v := <-newDone; v != "new" {
		t.Fatal(v)
	}
	if v := <-newCalls; v != `{"input":"one"}` {
		t.Fatal(v)
	}
	select {
	case <-newCalls:
		t.Fatal("request replayed")
	default:
	}
}
func TestQueueCancelFullAndTimeout(t *testing.T) {
	g := gatewayFor(t, "http://127.0.0.1:1")
	g.cfg.Wait = 30 * time.Millisecond
	control(t, g, "/hold?expected=a", 204)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := g.acquire(ctx, 200000); done <- err }()
	for {
		g.mu.Lock()
		n := len(g.queue)
		g.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := g.acquire(context.Background(), 200000); err == nil {
		t.Fatal("byte limit ignored")
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatal(err)
	}
	if _, err := g.acquire(context.Background(), 1); err == nil {
		t.Fatal("deadline ignored")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.queue) != 0 || g.bytes != 0 || g.active != 0 {
		t.Fatal("leaked queue reservation")
	}
}
func TestTargetsStayLocal(t *testing.T) {
	for _, s := range []string{"http://example.com:80", "http://127.0.0.1:80/path", "https://127.0.0.1:80", "http://user:pass@127.0.0.1:80"} {
		if _, e := localTarget(s); e == nil {
			t.Fatal(s)
		}
	}
}

func TestDurableTargetAndHold(t *testing.T) {
	c := Config{Target: "http://127.0.0.1:10001", Release: "a", MaxPending: 2, MaxActive: 2, MaxBody: 1000, MaxQueuedBytes: 100000, Wait: time.Second, StatePath: filepath.Join(t.TempDir(), "gateway.state")}
	g, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	control(t, g, "/hold?expected=a", 204)
	control(t, g, "/switch?expected=a&next=b&target=http://127.0.0.1:10002", 204)
	restarted, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.release != "b" || restarted.target.Port() != "10002" || !restarted.held {
		t.Fatal("restart lost committed release or hold")
	}
}

func waitGateway(t *testing.T, g *Gateway, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		g.mu.Lock()
		ok := predicate()
		g.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway state did not converge")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOverlapServesNewRequestsBeforeOldStreamFinishes(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/config" {
			io.WriteString(w, "old-management")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: old\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-finish
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "new:"+r.URL.Path) }))
	defer b.Close()
	g, err := New(Config{Target: a.URL, Release: "a", Overlap: true, MaxPending: 1, MaxActive: 8, MaxBody: 100000, MaxQueuedBytes: 800000, Wait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ingress := httptest.NewServer(g)
	defer ingress.Close()
	defer finishOnce.Do(func() { close(finish) })
	client := &http.Client{Timeout: 2 * time.Second}
	oldDone := make(chan string, 1)
	go func() {
		r, err := client.Get(ingress.URL + "/v1/responses")
		if err != nil {
			oldDone <- err.Error()
			return
		}
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		oldDone <- string(body)
	}()
	select {
	case <-started:
	case value := <-oldDone:
		t.Fatal("old stream did not start:", value)
	case <-time.After(2 * time.Second):
		t.Fatal("old stream did not start")
	}
	control(t, g, "/switch-live?expected=stale&next=b&target="+b.URL, 409)
	control(t, g, "/switch-live?expected=a&next=b&target=http://example.com:80", 400)
	control(t, g, "/switch-live?expected=a&next=b&target="+b.URL, 204)
	request := func(path string) string {
		t.Helper()
		r, err := client.Get(ingress.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("request %s: %d %v", path, r.StatusCode, err)
		}
		return string(body)
	}
	if got := request("/v1/models"); got != "new:/v1/models" {
		t.Fatal(got)
	}
	select {
	case value := <-oldDone:
		t.Fatal("old stream ended before release:", value)
	default:
	}
	if got := request("/admin/config"); got != "old-management" {
		t.Fatal(got)
	}
	waitGateway(t, g, func() bool { return g.releases["a"].RelayActive == 1 && g.releases["a"].ManagementActive == 0 })
	control(t, g, "/hold-management?expected=a", 204)
	managementDone := make(chan string, 1)
	go func() {
		r, err := client.Get(ingress.URL + "/admin/config")
		if err != nil {
			managementDone <- err.Error()
			return
		}
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		managementDone <- string(body)
	}()
	waitGateway(t, g, func() bool { return len(g.queue) == 1 })
	if got := request("/v1/models"); got != "new:/v1/models" {
		t.Fatal("management hold blocked relay:", got)
	}
	control(t, g, "/switch-management?expected=a&next=b", 204)
	control(t, g, "/resume-management?expected=b", 204)
	if got := <-managementDone; got != "new:/admin/config" {
		t.Fatal(got)
	}
	control(t, g, "/retire?expected=b&release=a", 409)
	finishOnce.Do(func() { close(finish) })
	if got := <-oldDone; !strings.Contains(got, "data: old") || !strings.Contains(got, "[DONE]") {
		t.Fatal(got)
	}
	waitGateway(t, g, func() bool { return g.releases["a"].RelayActive == 0 })
	control(t, g, "/retire?expected=b&release=a", 204)
	control(t, g, "/complete?expected=b", 204)
}

func TestOverlapPersistenceFailureAndRecovery(t *testing.T) {
	c := Config{Target: "http://127.0.0.1:10001", Release: "a", Overlap: true, MaxPending: 2, MaxActive: 2, MaxBody: 1000, MaxQueuedBytes: 100000, Wait: time.Second, StatePath: filepath.Join(t.TempDir(), "gateway.state")}
	g, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	original := g.cfg.StatePath
	g.cfg.StatePath = filepath.Join(t.TempDir(), "missing", "state")
	control(t, g, "/switch-live?expected=a&next=b&target=http://127.0.0.1:10002", 500)
	if g.release != "a" || g.held || len(g.releases) != 1 {
		t.Fatal("failed precommit changed routing")
	}
	g.cfg.StatePath = original
	g.syncStateDir = func(string) error { return errors.New("injected post-rename durability failure") }
	control(t, g, "/switch-live?expected=a&next=b&target=http://127.0.0.1:10002", 500)
	if !g.recoveryRequired || !g.held {
		t.Fatal("ambiguous commit did not fail closed")
	}
	restarted, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.recoveryRequired || !restarted.releases["a"].Unknown {
		t.Fatal("lost old counters treated as drained")
	}
	w := httptest.NewRecorder()
	restarted.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	if w.Code != 503 {
		t.Fatal("recovery gateway forwarded a request")
	}
	control(t, restarted, "/resume?expected=b", 409)
	control(t, restarted, "/switch-management?expected=a&next=b", 409)
	control(t, restarted, "/switch-management?expected=a&next=b&confirmed_drained=true", 204)
	control(t, restarted, "/retire?expected=b&release=a", 409)
	control(t, restarted, "/retire?expected=b&release=a&confirmed_drained=true", 204)
	control(t, restarted, "/resume?expected=b&confirm_recovery=true", 204)
	control(t, restarted, "/resume-management?expected=b", 204)
	control(t, restarted, "/complete?expected=b", 204)
	stable, err := New(c)
	if err != nil || stable.recoveryRequired || stable.release != "b" {
		t.Fatal("completed release did not restart normally", err)
	}
}

func TestOverlapMustBeEnabled(t *testing.T) {
	g := gatewayFor(t, "http://127.0.0.1:10001")
	control(t, g, "/switch-live?expected=a&next=b&target=http://127.0.0.1:10002", 409)
}

func TestLiveRollbackKeepsAlreadyAdmittedCandidateRequestsPinned(t *testing.T) {
	g, err := New(Config{Target: "http://127.0.0.1:10001", Release: "a", Overlap: true, MaxPending: 2, MaxActive: 4, MaxBody: 1000, MaxQueuedBytes: 100000, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	control(t, g, "/switch-live?expected=a&next=b&target=http://127.0.0.1:10002", 204)
	candidate, err := g.acquireRoute(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	control(t, g, "/rollback-live?expected=b&next=a", 204)
	restored, err := g.acquireRoute(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.release != "b" || candidate.target.Port() != "10002" || restored.release != "a" || restored.target.Port() != "10001" {
		t.Fatal("rollback rebound an existing request")
	}
	control(t, g, "/retire?expected=a&release=b", 409)
	g.leave(candidate.release, false)
	control(t, g, "/retire?expected=a&release=b", 204)
	g.leave(restored.release, false)
	control(t, g, "/complete?expected=a", 204)
}

func TestConcurrentLiveSwitchHasOneWinnerAndRetainsOldReservation(t *testing.T) {
	g, err := New(Config{Target: "http://127.0.0.1:10001", Release: "a", Overlap: true, MaxPending: 2, MaxActive: 4, MaxBody: 1000, MaxQueuedBytes: 100000, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	old, err := g.acquireRoute(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer g.leave(old.release, false)
	start := make(chan struct{})
	results := make(chan int, 2)
	for _, path := range []string{
		"/switch-live?expected=a&next=b&target=http://127.0.0.1:10002",
		"/switch-live?expected=a&next=c&target=http://127.0.0.1:10003",
	} {
		go func(path string) {
			<-start
			w := httptest.NewRecorder()
			g.Control().ServeHTTP(w, httptest.NewRequest("POST", path, nil))
			results <- w.Code
		}(path)
	}
	close(start)
	winners, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		switch code := <-results; code {
		case 204:
			winners++
		case 409:
			conflicts++
		default:
			t.Fatalf("unexpected switch status %d", code)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatal("concurrent switch did not compare-and-swap", winners, conflicts)
	}
	next, err := g.acquireRoute(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer g.leave(next.release, false)
	g.mu.Lock()
	defer g.mu.Unlock()
	if old.release != "a" || next.release == "a" || len(g.releases) != 2 || g.releases["a"].RelayActive != 1 || g.releases[next.release].RelayActive != 1 {
		t.Fatal("concurrent switch lost per-release admission accounting")
	}
}
