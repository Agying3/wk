package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/responsehistory"
)

func TestResponseHistoryConversationCapacityReported(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			p := responsehistory.Default()
			p.MaxEntries = 2
			s, err := responsehistory.OpenSQLite(filepath.Join(t.TempDir(), "responses.db"), p, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			calls := 0
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				calls++
				return 200, strings.ReplaceAll(sseOK, "chatcmpl-1", fmt.Sprintf("capacity-%d", calls)), true
			})
			h := newTestHandler(t, Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "fake", ExpiresAt: 9999999999}), Upstream: up, ResponseStore: s, ResponseHistory: p})
			for turn := 1; turn <= 3; turn++ {
				request := map[string]any{"model": "glm-5.2", "input": "next", "stream": stream}
				if turn > 1 {
					request["previous_response_id"] = fmt.Sprintf("capacity-%d", turn-1)
				}
				body, _ := json.Marshal(request)
				rec := doResponsesRequest(h, string(body))
				if rec.Code != 200 {
					t.Fatalf("turn %d: %d %s", turn, rec.Code, rec.Body)
				}
				if turn == 3 && (!strings.Contains(rec.Body.String(), `"history_stored":false`) || !strings.Contains(rec.Body.String(), "response_history_capacity_exceeded")) {
					t.Fatalf("capacity failure hidden: %s", rec.Body)
				}
				if stream && !strings.Contains(rec.Body.String(), "response.completed") {
					t.Fatal("generated output was lost")
				}
			}
			if _, _, ok := h.loadResponseHistory("capacity-2"); !ok {
				t.Fatal("capacity rejection destroyed the existing chain")
			}
			if _, ok := s.LoadResponse("capacity-3"); ok {
				t.Fatal("oversized conversation append persisted")
			}
		})
	}
}

func TestResponseHistoryConversationProtectedThroughoutRequest(t *testing.T) {
	p := responsehistory.Default()
	p.MaxEntries = 3
	s, err := responsehistory.OpenSQLite(filepath.Join(t.TempDir(), "responses.db"), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	peer := newTestHandler(t, Config{ResponseStore: s, ResponseHistory: p})
	for _, item := range []struct{ id, parent string }{{"a", ""}, {"a1", "a"}, {"b", ""}} {
		if err := peer.storeResponse(item.id, historyMessages(item.id), "route", item.parent); err != nil {
			t.Fatal(err)
		}
	}
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		// Simulate another instance writing while the upstream request is active.
		if err := peer.storeResponse("c", historyMessages("competitor"), "other-route", ""); err != nil {
			t.Error(err)
		}
		if _, ok := s.LoadResponse("a"); !ok {
			t.Error("in-flight root was evicted")
		}
		if _, ok := s.LoadResponse("b"); ok {
			t.Error("unprotected conversation was not evicted")
		}
		return 200, strings.ReplaceAll(sseOK, "chatcmpl-1", "a2"), true
	})
	h := newTestHandler(t, Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "fake", ExpiresAt: 9999999999}), Upstream: up, ResponseStore: s, ResponseHistory: p})
	rec := doResponsesRequest(h, `{"model":"glm-5.2","input":"continue","previous_response_id":"a1"}`)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "history_error") {
		t.Fatalf("continuation: %d %s", rec.Code, rec.Body)
	}
	if messages, _, ok := h.loadResponseHistory("a2"); !ok || len(messages) != 6 {
		t.Fatal("active conversation did not survive competing writes")
	}
	if _, ok := s.LoadResponse("c"); ok {
		t.Fatal("capacity was not enforced after the active conversation grew")
	}
}

func TestResponseHistoryDeletedConversationRejectsBeforeUpstream(t *testing.T) {
	p := responsehistory.Default()
	p.MaxEntries = 1
	s, err := responsehistory.OpenSQLite(filepath.Join(t.TempDir(), "responses.db"), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 200, sseOK, true })
	h := newTestHandler(t, Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "fake", ExpiresAt: 9999999999}), Upstream: up, ResponseStore: s, ResponseHistory: p})
	h.storeResponse("old", historyMessages("old"), "old-route", "")
	time.Sleep(time.Millisecond)
	h.storeResponse("new", historyMessages("new"), "new-route", "")
	rec := doResponsesRequest(h, `{"model":"glm-5.2","input":"continue","previous_response_id":"old"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "previous_response_not_found") || calls != 0 {
		t.Fatalf("deleted continuation: %d, upstream calls=%d", rec.Code, calls)
	}
}

func TestResponseHistoryIdleTTLDoesNotExpireActiveAncestors(t *testing.T) {
	p := responsehistory.Default()
	s, err := responsehistory.OpenSQLite(filepath.Join(t.TempDir(), "responses.db"), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	created := now.Add(-30 * time.Minute)
	old := storedResponseWire{Messages: historyMessages("old ancestor"), RouteKey: "route", CreatedAt: created.UnixMilli(), ExpiresAt: created.Add(time.Hour).UnixMilli()}
	raw, _ := json.Marshal(old)
	if ok, err := s.PersistResponse("old-root", raw, time.Hour); !ok || err != nil {
		t.Fatalf("old root: %v %v", ok, err)
	}
	h := newTestHandler(t, Config{ResponseStore: s})
	if err := h.storeResponse("active-child", historyMessages("recent continuation"), "route", "old-root"); err != nil {
		t.Fatal(err)
	}
	p.TTLSeconds = 60
	if !h.setResponseHistoryPolicy(p) {
		t.Fatal("idle TTL update failed")
	}
	got, _, ok := h.loadResponseHistory("active-child")
	if !ok || len(got) != 4 || got[0]["content"] != "old ancestor" {
		t.Fatal("per-turn timestamp overrode the active conversation deadline")
	}
}
