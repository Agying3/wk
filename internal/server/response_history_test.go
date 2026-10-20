package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if got["ttl_seconds"] != float64(3600) || got["max_entries"] != float64(1024) || got["max_storage_mb"] != float64(64) || got["max_entry_mb"] != float64(32) {
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
	want := responsehistory.Policy{TTLSeconds: 604800, MaxEntries: 100000, MaxStorageMB: 256, MaxEntryMB: 32}
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
		`{"max_entry_mb":0}`, `{"max_entry_mb":-1}`, `{"max_entry_mb":1000000001}`,
		`{"max_entry_mb":1.5}`, `{"max_entry_mb":"32"}`,
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
	p := responsehistory.Policy{TTLSeconds: 604800, MaxEntries: 2048, MaxStorageMB: 2, MaxEntryMB: 32}
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

func TestResponseHistoryIndependentEntryLimitAndImmediateResize(t *testing.T) {
	h, path := adminConfigTestEnv(t)
	update := func(patch map[string]any) {
		doAdminConfig(t, h, http.MethodPost, map[string]any{
			"checkin_hours": []int{9}, "keepalive_hours": []int{22}, "response_history": patch,
		})
	}
	update(map[string]any{"max_storage_mb": 8, "max_entry_mb": 2})
	h.storeResponse("small-a", historyMessages(strings.Repeat("a", 300000)), "route", "")
	h.storeResponse("large", historyMessages(strings.Repeat("b", 1500000)), "route", "")
	h.storeResponse("small-b", historyMessages(strings.Repeat("c", 300000)), "route", "")
	if len(h.responseHistory) != 3 {
		t.Fatal("records below the independent entry limit were not stored")
	}
	before := h.responseBytes
	h.storeResponse("too-large", historyMessages(strings.Repeat("x", 2100000)), "route", "")
	if h.responseBytes != before || len(h.responseHistory) != 3 {
		t.Fatal("oversized insertion evicted useful records despite a free total budget")
	}
	// Editing only the entry limit must preserve the independent total budget.
	update(map[string]any{"max_entry_mb": 1})
	if h.responseHistory["large"] != nil || h.responseHistory["small-a"] == nil || h.responseHistory["small-b"] == nil {
		t.Fatal("shrinking entry limit did not prune exactly the oversized record")
	}
	var used int64
	for _, r := range h.responseHistory {
		used += int64(len(r.raw))
	}
	if used != h.responseBytes {
		t.Fatal("entry pruning broke byte accounting")
	}
	var saved struct {
		History responsehistory.Policy `json:"response_history"`
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.History.MaxEntryMB != 1 || saved.History.MaxStorageMB != 8 {
		t.Fatalf("independent limits were not persisted: %+v err=%v", saved, err)
	}
	update(map[string]any{"max_entry_mb": 4})
	if _, _, ok := h.loadResponseHistory("large"); ok {
		t.Fatal("raising the entry limit revived discarded history")
	}
	update(map[string]any{"max_storage_mb": 1})
	if h.currentResponseHistoryPolicy().MaxEntryMB != 4 {
		t.Fatal("editing total budget changed entry limit")
	}
	h.storeResponse("new-a", historyMessages(strings.Repeat("d", 600000)), "route", "")
	h.storeResponse("new-b", historyMessages(strings.Repeat("e", 600000)), "route", "")
	if h.responseHistory["new-b"] == nil || h.responseHistory["new-a"] != nil || h.responseBytes > 1000000 {
		t.Fatal("total budget was bypassed by a larger per-entry limit")
	}
}

func TestResponseHistoryEntryBoundaryAndRemoteRefill(t *testing.T) {
	p := responsehistory.Default()
	p.MaxEntryMB = 1
	h := newTestHandler(t, Config{ResponseHistory: p})
	now := time.Now()
	if !h.cacheResponse("fits", make([]byte, 1000000), now, now.Add(time.Hour)) {
		t.Fatal("exact decimal MB boundary was rejected")
	}
	if h.cacheResponse("oversized", make([]byte, 1000001), now, now.Add(time.Hour)) || h.responseBytes != 1000000 {
		t.Fatal("entry boundary accepted an oversized payload or evicted existing data")
	}
	wire := storedResponseWire{Messages: historyMessages(strings.Repeat("x", 1100000)), RouteKey: "route", CreatedAt: now.UnixMilli(), ExpiresAt: now.Add(time.Hour).UnixMilli()}
	raw, _ := json.Marshal(wire)
	remote := newTestHandler(t, Config{ResponseHistory: p, ResponseStore: staticHistoryStore{raw: raw}})
	if _, _, ok := remote.loadResponseHistory("large"); ok || remote.responseBytes != 0 {
		t.Fatal("remote refill bypassed the entry limit")
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
	for _, disk := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("disk=%v/stream=%v", disk, stream), func(t *testing.T) {
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
				cfg := Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "fake", ExpiresAt: 9999999999}), Upstream: up}
				path := filepath.Join(t.TempDir(), "responses.db")
				var archive *responsehistory.SQLStore
				newHandler := func() authedHandler {
					if disk {
						if archive != nil {
							archive.Close()
						}
						var err error
						archive, err = responsehistory.OpenSQLite(path, responsehistory.Default(), nil)
						if err != nil {
							t.Fatal(err)
						}
						cfg.ResponseStore = archive
						t.Cleanup(func() { archive.Close() })
					}
					return newTestHandler(t, cfg)
				}
				h := newHandler()
				for turn := 1; turn <= 3; turn++ {
					if disk && turn > 1 {
						h = newHandler()
					}
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
}

func TestResponseHistoryDiskMetadataAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses.db")
	p := responsehistory.Default()
	s, err := responsehistory.OpenSQLite(path, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, Config{ResponseStore: s})
	messages := []map[string]any{
		{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": strings.Repeat("中文\n", 10000)},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}},
		}},
		{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "f", "arguments": `{ "x": "a\\b" }`}},
		}},
	}
	want, _ := json.Marshal(messages)
	h.storeResponse("disk-parent", messages, "stable-route", "")
	if r := h.responseHistory["disk-parent"]; r == nil || r.raw != nil || r.size == 0 || h.responseBytes != r.size {
		t.Fatal("body retained in index")
	}
	messages[0]["content"] = "mutated after save"
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = responsehistory.OpenSQLite(path, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h = newTestHandler(t, Config{ResponseStore: s})
	h.storeResponse("disk-child", historyMessages("next turn"), "stable-route", "disk-parent")
	got, route, ok := h.loadResponseHistory("disk-child")
	if !ok || route != "stable-route" || len(got) != 4 {
		t.Fatalf("restart continuation: %s %v %d", route, ok, len(got))
	}
	actual, _ := json.Marshal(got[:2])
	if string(actual) != string(want) {
		t.Fatal("image/tool/text history changed")
	}
	for _, r := range h.responseHistory {
		if r.raw != nil {
			t.Fatal("refill retained body")
		}
	}
	// Another instance can evict an entry; an old RAM index cannot serve it.
	p.MaxEntries = 1
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.loadResponseRecord("disk-parent"); ok {
		t.Fatal("RAM index bypassed database eviction")
	}
}
