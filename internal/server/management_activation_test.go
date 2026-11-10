package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/haozhuma"
)

type managementGrowthStore struct {
	claims []GrowthLedgerClaim
	err    error
}

func TestActivateSMSClientRetainsPendingOrphanActions(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"test-active-token"}`))
	}))
	defer srv.Close()
	c, err := haozhuma.BuildStandby(haozhuma.Settings{User: "test-user", Pass: "test-password", Sid: "123"}, srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	a := &AutoEnroller{hzm: c, held: map[string]bool{"pending-phone": true}, blacklistPending: map[string]string{"pending-phone": "original-project"}}
	h := &Handler{cfg: Config{AutoEnroll: a}}
	if err = h.ActivateSMSClient(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !a.held["pending-phone"] || a.blacklistPending["pending-phone"] != "original-project" || a.clientPreparing {
		t.Fatal("SMS activation discarded pending orphan actions")
	}
	if calls.Load() != 1 || a.hzm == c {
		t.Fatal("SMS standby was not activated")
	}
	if err = h.ActivateSMSClient(context.Background()); err != nil || calls.Load() != 1 {
		t.Fatal("SMS activation was not idempotent")
	}
}
func (s *managementGrowthStore) RecordGrowthClaim(c GrowthLedgerClaim) error {
	s.claims = append(s.claims, c)
	return s.err
}
func (s *managementGrowthStore) LoadGrowthClaims() ([]GrowthLedgerClaim, error) {
	return append([]GrowthLedgerClaim(nil), s.claims...), s.err
}

func TestActivateManagementReloadsFinalOwnerSnapshotsWithoutDispatch(t *testing.T) {
	dir := t.TempDir()
	heldPath, watchPath := filepath.Join(dir, "held.json"), filepath.Join(dir, "watch.json")
	store := &managementGrowthStore{}
	a := &AutoEnroller{ledgerPath: heldPath, held: map[string]bool{"stale-phone": true}, blacklistPending: map[string]string{}}
	w := &UIDWatcher{statePath: watchPath, state: watcherState{PerUID: map[string]*watchUIDState{}, BudgetRemaining: 100}}
	h := &Handler{cfg: Config{RelayOnly: true, GrowthLedgerStore: store, AutoEnroll: a, UIDWatcher: w}, growthLedger: newGrowthLedger("", store)}
	// The old owner continues updating durable state after candidate construction.
	store.claims = []GrowthLedgerClaim{{UID: "test-account", TaskCode: "final-claim", Credit: 3, ClaimedAtUnix: 1}}
	if err := os.WriteFile(heldPath, []byte(`{"version":2,"phones":["final-phone"],"blacklists":{"final-phone":"test-project"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := watcherState{PerUID: map[string]*watchUIDState{}, BudgetRemaining: 71, SpentTotal: 29, TargetUID: "final-target"}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(watchPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = h.ActivateManagementState(); err != nil {
		t.Fatal(err)
	}
	if h.growthLedger.byUID["test-account"] == nil {
		t.Fatal("final growth claim missing")
	}
	if len(a.held) != 1 || !a.held["final-phone"] || a.blacklistPending["final-phone"] != "test-project" {
		t.Fatal("held-number snapshot not reloaded")
	}
	if w.state.BudgetRemaining != 71 || w.state.TargetUID != "final-target" || w.loopRunning || a.running {
		t.Fatal("watcher snapshot lost or background dispatched")
	}
	beforeHeld, err := os.ReadFile(heldPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeHeld) != `{"version":2,"phones":["final-phone"],"blacklists":{"final-phone":"test-project"}}` {
		t.Fatal("activation rewrote held ledger")
	}
	// A repeated promotion callback cannot overwrite newly acquired local work.
	a.held["new-phone"] = true
	if err = h.ActivateManagementState(); err != nil {
		t.Fatal(err)
	}
	if !a.held["new-phone"] {
		t.Fatal("repeat activation reset active management state")
	}
}

func TestActivateManagementReloadFailureLeavesStateUnpublished(t *testing.T) {
	store := &managementGrowthStore{err: errors.New("database unavailable")}
	old := &growthLedger{byUID: map[string]*ledgerAccount{"existing": {UID: "existing"}}}
	h := &Handler{cfg: Config{RelayOnly: true, GrowthLedgerStore: store}, growthLedger: old}
	if err := h.ActivateManagementState(); err == nil {
		t.Fatal("promoted with unavailable growth authority")
	}
	if h.growthLedger != old || h.webAuthority.managementActive {
		t.Fatal("partial management activation published")
	}
	store.err = nil
	watchPath := filepath.Join(t.TempDir(), "watch.json")
	if err := os.WriteFile(watchPath, []byte(`{"per_uid":{"bad":null}}`), 0600); err != nil {
		t.Fatal(err)
	}
	h.cfg.UIDWatcher = &UIDWatcher{statePath: watchPath}
	if err := h.ActivateManagementState(); err == nil {
		t.Fatal("promoted with invalid watcher snapshot")
	}
	if h.growthLedger != old || h.webAuthority.managementActive {
		t.Fatal("partial management activation published")
	}
}
