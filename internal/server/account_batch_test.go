package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

type batchPersister struct {
	calls int
	rows  map[string][]byte
	err   error
}

func (s *batchPersister) UpsertPoolAccounts(rows map[string][]byte) error {
	s.calls++
	if s.err != nil {
		return s.err
	}
	s.rows = rows
	return nil
}
func (s *batchPersister) DeletePoolAccounts([]string) error            { return nil }
func (s *batchPersister) LoadPoolAccounts() (map[string][]byte, error) { return nil, nil }

func TestAccountBatchCommits100AccountsOnce(t *testing.T) {
	h, _, _ := newGroupTestHandler(t)
	s := &batchPersister{}
	p := pool.New("", s)
	uids := make([]string, 100)
	for i := range uids {
		uids[i] = fmt.Sprintf("batch-%d", i)
		p.Add(&auth.Auth{UID: uids[i], AccessToken: "synthetic", ExpiresAt: 9999999999})
	}
	h.cfg.Pool = p
	w := transferRequest(t, h, "/admin/accounts/batch", accountBatchRequest{UIDs: uids, Action: "disable"})
	if w.Code != 200 || s.calls != 1 || len(s.rows) != 100 {
		t.Fatalf("code=%d commits=%d rows=%d body=%s", w.Code, s.calls, len(s.rows), w.Body.String())
	}
	for _, uid := range uids {
		var state struct {
			Disabled bool `json:"disabled"`
		}
		if err := json.Unmarshal(s.rows[uid], &state); err != nil || !state.Disabled {
			t.Fatalf("not persisted: %s %v", uid, err)
		}
	}
	for _, action := range []string{"clear-cooldown", "enable"} {
		before := s.calls
		w = transferRequest(t, h, "/admin/accounts/batch", accountBatchRequest{UIDs: []string{uids[0], uids[0], "missing"}, Action: action})
		var body struct {
			Results []accountBatchRow `json:"results"`
		}
		json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != 200 || s.calls != before+1 || len(body.Results) != 2 || body.Results[1].Status != "failed" {
			t.Fatal(w.Body.String())
		}
		status, _ := p.Status(uids[0])
		if status.Disabled != (action == "clear-cooldown") {
			t.Fatalf("incorrect disabled state after %s", action)
		}
	}
}

func TestAccountBatchPersistenceFailureIsExplicit(t *testing.T) {
	h, _, _ := newGroupTestHandler(t)
	s := &batchPersister{err: errors.New("synthetic failure")}
	h.cfg.Pool.SetStatePersister(s)
	w := transferRequest(t, h, "/admin/accounts/batch", accountBatchRequest{UIDs: []string{"u1"}, Action: "disable"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"stop":true`) || !strings.Contains(w.Body.String(), `"status":"failed"`) || !strings.Contains(w.Body.String(), "内存状态已变更") {
		t.Fatal(w.Body.String())
	}
	if !h.cfg.Pool.IsDisabled("u1") {
		t.Fatal("must report that memory changed")
	}
	s.err = nil
	if err := h.cfg.Pool.FlushWithError(); err != nil || len(s.rows) == 0 {
		t.Fatalf("failed changes not retained for persistence retry: %v", err)
	}
}

func TestAccountBatchGroupsMergeAndPartialFailure(t *testing.T) {
	h, _, _ := newGroupTestHandler(t)
	if err := h.cfg.Groups.CreateWithRegion("cn", "cn"); err != nil {
		t.Fatal(err)
	}
	h.cfg.Pool.Add(&auth.Auth{UID: "overseas", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	w := transferRequest(t, h, "/admin/accounts/batch", accountBatchRequest{UIDs: []string{"u1", "overseas", "missing"}, Action: "groups", Mode: "append", Groups: []string{"cn"}})
	var body struct {
		Results []accountBatchRow `json:"results"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 200 || len(body.Results) != 3 || body.Results[0].Status != "success" || body.Results[1].Status != "failed" || body.Results[2].Status != "failed" {
		t.Fatal(w.Body.String())
	}
	if got := h.cfg.Groups.AccountGroups("u1"); !reflect.DeepEqual(got, []string{"cn", "vip"}) {
		t.Fatal(got)
	}
	w = transferRequest(t, h, "/admin/accounts/batch", accountBatchRequest{UIDs: []string{"u1"}, Action: "groups", Mode: "remove", Groups: []string{"cn", "vip"}})
	if w.Code != 200 || !reflect.DeepEqual(h.cfg.Groups.AccountGroups("u1"), []string{"default"}) {
		t.Fatal(w.Body.String())
	}
}

func TestAccountBatchAuthorizationAndValidation(t *testing.T) {
	h, groupKey, _ := newGroupTestHandler(t)
	for _, key := range []string{"", groupKey} {
		r := httptest.NewRequest("POST", "/admin/accounts/batch", strings.NewReader(`{"uids":["u1"],"action":"disable"}`))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 || h.cfg.Pool.IsDisabled("u1") {
			t.Fatal("nonadmin accepted")
		}
	}
	for _, req := range []accountBatchRequest{
		{UIDs: nil, Action: "disable"}, {UIDs: make([]string, 101), Action: "disable"},
		{UIDs: []string{"u1", ""}, Action: "disable"}, {UIDs: []string{"u1"}, Action: "checkin"},
	} {
		w := transferRequest(t, h, "/admin/accounts/batch", req)
		if w.Code != 400 || h.cfg.Pool.IsDisabled("u1") {
			t.Fatalf("invalid request changed pool: %s", w.Body.String())
		}
	}
}
