package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/responsehistory"
)

func historyMessages(text string) []map[string]any {
	return []map[string]any{{"role": "user", "content": text}, {"role": "assistant", "content": "ok"}}
}

func TestResponseHistoryConfigAndImmediateResize(t *testing.T) {
	h, path := adminConfigTestEnv(t)
	got := doAdminConfig(t, h, http.MethodGet, nil)["response_history"].(map[string]any)
	if got["ttl_seconds"] != float64(3600) || got["max_entries"] != float64(1024) || got["max_storage_mb"] != float64(64) {
		t.Fatalf("defaults: %v", got)
	}
	for _, id := range []string{"a", "b", "c"} {
		h.storeResponse(id, historyMessages(id), "route", "")
	}
	doAdminConfig(t, h, http.MethodPost, map[string]any{
		"checkin_hours": []int{9}, "keepalive_hours": []int{22},
		"response_history": map[string]any{"ttl_seconds": 604800, "max_entries": 2, "max_storage_mb": 256},
	})
	if _, _, ok := h.loadResponseHistory("a"); ok {
		t.Fatal("oldest entry survived shrinking count")
	}
	if _, _, ok := h.loadResponseHistory("c"); !ok {
		t.Fatal("newest entry lost")
	}
	// Partial edits merge with the active values and persist all effective fields.
	doAdminConfig(t, h, http.MethodPost, map[string]any{
		"checkin_hours": []int{9}, "keepalive_hours": []int{22},
		"response_history": map[string]any{"max_entries": 100000},
	})
	want := responsehistory.Policy{TTLSeconds: 604800, MaxEntries: 100000, MaxStorageMB: 256}
	if p := h.currentResponseHistoryPolicy(); p != want {
		t.Fatalf("policy=%+v want %+v", p, want)
	}
	raw, _ := os.ReadFile(path)
	var doc struct {
		History responsehistory.Policy `json:"response_history"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.History != want {
		t.Fatalf("persisted policy=%+v err=%v", doc, err)
	}
	if _, _, ok := h.loadResponseHistory("a"); ok {
		t.Fatal("increasing limits revived evicted data")
	}
}

func TestResponseHistoryRejectsInvalidConfigWithoutChanges(t *testing.T) {
	h, path := adminConfigTestEnv(t)
	before, _ := os.ReadFile(path)
	for _, bad := range []string{
		`{"ttl_seconds":0}`, `{"ttl_seconds":-1}`, `{"ttl_seconds":3153600001}`,
		`{"max_entries":0}`, `{"max_entries":-1}`, `{"max_entries":1000000001}`,
		`{"max_storage_mb":0}`, `{"max_storage_mb":-1}`, `{"max_storage_mb":1000000001}`,
		`{"max_entries":1.5}`, `{"max_storage_mb":"64"}`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(
			`{"checkin_hours":[9],"keepalive_hours":[22],"response_history":`+bad+`}`)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", bad, rec.Code, rec.Body)
		}
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || h.currentResponseHistoryPolicy() != responsehistory.Default() {
		t.Fatal("invalid update mutated config")
	}
}

func TestResponseHistoryLargerLimitsAndTotalBudget(t *testing.T) {
	p := responsehistory.Policy{TTLSeconds: 604800, MaxEntries: 2048, MaxStorageMB: 2}
	h := newTestHandler(t, Config{ResponseHistory: p})
	for i := 0; i < 1100; i++ {
		h.storeResponse(time.Unix(int64(i), 0).String(), historyMessages("hello"), "route", "")
	}
	if len(h.responseHistory) != 1100 {
		t.Fatalf("old hardcoded count still limits storage: %d", len(h.responseHistory))
	}
	h.storeResponse("large-a", historyMessages(strings.Repeat("x", 600000)), "route", "")
	h.storeResponse("large-b", historyMessages(strings.Repeat("y", 600000)), "route", "")
	if r := h.responseHistory["large-b"]; r.expiresAt.Sub(r.createdAt) != 7*24*time.Hour {
		t.Fatal("configured retention ignored")
	}
	p.MaxStorageMB = 1
	h.setResponseHistoryPolicy(p)
	if h.responseBytes > 1000000 {
		t.Fatalf("budget exceeded: %d", h.responseBytes)
	}
	if _, _, ok := h.loadResponseHistory("large-a"); ok {
		t.Fatal("old large record survived budget reduction")
	}
	if _, _, ok := h.loadResponseHistory("large-b"); !ok {
		t.Fatal("newest fitting record missing")
	}
	before := h.responseBytes
	h.storeResponse("oversized", historyMessages(strings.Repeat("z", 1100000)), "route", "")
	if h.responseBytes != before || h.responseHistory["large-b"] == nil {
		t.Fatal("oversized insertion evicted useful history")
	}
}

func TestResponseHistoryExpiryAndAncestorLoss(t *testing.T) {
	h := newTestHandler(t, Config{})
	h.storeResponse("parent", historyMessages("one"), "route", "")
	h.storeResponse("child", historyMessages("two"), "route", "parent")
	messages, key, ok := h.loadResponseHistory("child")
	if !ok || len(messages) != 4 || key != "route" {
		t.Fatalf("chain: messages=%v route=%s ok=%v", messages, key, ok)
	}
	// Mutating a restored transcript must never alter stored history.
	messages[0]["content"] = "mutated"
	messages, _, _ = h.loadResponseHistory("child")
	if messages[0]["content"] != "one" {
		t.Fatal("stored history was mutated")
	}
	h.responsesMu.Lock()
	h.responseHistory["parent"].createdAt = time.Now().Add(-time.Hour)
	h.responsesMu.Unlock()
	p := h.currentResponseHistoryPolicy()
	p.TTLSeconds = 60
	h.setResponseHistoryPolicy(p)
	if _, _, ok := h.loadResponseHistory("child"); ok {
		t.Fatal("chain with expired parent restored")
	}
	if h.responseHistory["parent"] != nil {
		t.Fatal("shorter retention did not expire existing parent")
	}
}

func TestResponseHistoryConcurrentResize(t *testing.T) {
	h := newTestHandler(t, Config{})
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if worker == 0 {
					p := responsehistory.Default()
					p.MaxEntries = 10 + i
					h.setResponseHistoryPolicy(p)
				} else {
					id := time.Unix(int64(worker*100+i), 0).String()
					h.storeResponse(id, historyMessages("hello"), "route", "")
					h.loadResponseHistory(id)
				}
			}
		}(worker)
	}
	wg.Wait()
	h.responsesMu.Lock()
	defer h.responsesMu.Unlock()
	var total int64
	for _, r := range h.responseHistory {
		total += int64(len(r.raw))
	}
	if total != h.responseBytes || len(h.responseOrder) != len(h.responseHistory) || len(h.responseExpiry) != len(h.responseHistory) || len(h.responseHistory) > h.responsePolicy.MaxEntries {
		t.Fatal("concurrent accounting mismatch")
	}
}

type staticHistoryStore struct{ raw []byte }

func (s staticHistoryStore) SaveResponse(string, []byte, time.Duration) {}
func (s staticHistoryStore) LoadResponse(string) ([]byte, bool)         { return s.raw, true }

func TestResponseHistoryRemoteRefillPreservesExpiration(t *testing.T) {
	now := time.Now()
	wire := storedResponseWire{Messages: historyMessages("old"), RouteKey: "route", CreatedAt: now.Add(-time.Minute).UnixMilli(), ExpiresAt: now.Add(time.Hour).UnixMilli()}
	raw, _ := json.Marshal(wire)
	p := responsehistory.Default()
	p.TTLSeconds = 10
	h := newTestHandler(t, Config{ResponseHistory: p, ResponseStore: staticHistoryStore{raw: raw}})
	// Simulate an expired memory entry with a still-present remote copy.
	h.cacheResponse("old", raw, now, now.Add(time.Hour))
	h.responsesMu.Lock()
	h.responseHistory["old"].expiresAt = now.Add(-time.Second)
	h.responsesMu.Unlock()
	if _, _, ok := h.loadResponseHistory("old"); ok {
		t.Fatal("remote refill renewed expired history")
	}
	if h.responseHistory["old"] != nil {
		t.Fatal("expired refill was cached")
	}
}

func TestResponseHistoryHTTPContinuation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			count := 0
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				count++
				return 200, strings.ReplaceAll(sseOK, "chatcmpl-1", fmt.Sprintf("history-%d", count)), true
			})
			base := up.HTTP.Transport
			var upstreamMessages []any
			up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var doc map[string]any
				if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
					t.Fatal(err)
				}
				upstreamMessages, _ = doc["messages"].([]any)
				return base.RoundTrip(r)
			})
			h := newTestHandler(t, Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "fake", ExpiresAt: 9999999999}), Upstream: up})
			for turn := 1; turn <= 3; turn++ {
				request := map[string]any{"model": "glm-5.2", "input": fmt.Sprintf("q%d", turn), "stream": stream, "instructions": "current instruction"}
				if turn > 1 {
					request["previous_response_id"] = fmt.Sprintf("history-%d", turn-1)
				}
				body, _ := json.Marshal(request)
				rec := doResponsesRequest(h, string(body))
				if rec.Code != 200 {
					t.Fatalf("turn %d: %d %s", turn, rec.Code, rec.Body)
				}
				if len(upstreamMessages) != turn*2 {
					t.Fatalf("turn %d restored %d messages: %v", turn, len(upstreamMessages), upstreamMessages)
				}
				if first := upstreamMessages[0].(map[string]any); first["content"] != "current instruction" {
					t.Fatal("instructions misplaced")
				}
				if stream && !strings.Contains(rec.Body.String(), "response.completed") {
					t.Fatal("stream did not complete")
				}
			}
		})
	}
}
