package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
)

// ActivateSMSClient is deliberately separate from the read-only snapshot reload.
// The caller must hold management ownership before this may log in a supplier.
func (h *Handler) ActivateSMSClient(ctx context.Context) error {
	h.webAuthority.managementMu.Lock()
	defer h.webAuthority.managementMu.Unlock()
	a := h.cfg.AutoEnroll
	if a == nil {
		return nil
	}
	a.mu.Lock()
	if a.running || a.fetchLease || a.clientPreparing {
		a.mu.Unlock()
		return errors.New("cannot activate SMS client while enrollment is active")
	}
	current := a.hzm
	if current == nil {
		a.mu.Unlock()
		return nil
	}
	a.clientPreparing = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.clientPreparing = false; a.mu.Unlock() }()
	active, err := current.ActivateStandby(ctx)
	if err != nil {
		return errors.New("cannot activate standby SMS supplier client")
	}
	if active == current {
		return nil
	}
	// This is the same configured supplier identity, with its deferred login
	// completed. Keep pending orphan actions; ReplaceClient deliberately rejects
	// configuration changes when held numbers exist and is unsuitable here.
	a.mu.Lock()
	if a.hzm != current {
		a.mu.Unlock()
		return errors.New("SMS client changed during activation")
	}
	a.hzm = active
	a.mu.Unlock()
	h.cfg.HaozhumaClient = active
	return nil
}

// ActivateManagementState is called after acquiring the exclusive management
// lease, before admitting management requests or starting background workers.
// A standby's constructor snapshot may predate the previous owner's final
// writes. Reload those snapshots without dispatching any upstream work.
func (h *Handler) ActivateManagementState() error {
	h.webAuthority.managementMu.Lock()
	defer h.webAuthority.managementMu.Unlock()
	if h.webAuthority.managementActive {
		return nil
	}
	if !h.cfg.RelayOnly {
		h.webAuthority.managementActive = true
		return nil
	}

	ledger, err := h.reloadGrowthLedger()
	if err != nil {
		return err
	}
	var held map[string]bool
	var blacklists map[string]string
	a := h.cfg.AutoEnroll
	if a != nil {
		a.mu.Lock()
		busy, path := a.running || a.fetchLease || a.clientPreparing, a.ledgerPath
		a.mu.Unlock()
		if busy {
			return errors.New("cannot activate management with active auto-enrollment")
		}
		held, blacklists, err = readHeldManagementLedger(path)
		if err != nil {
			return err
		}
	}
	var snapshot watcherState
	w := h.cfg.UIDWatcher
	if w != nil {
		w.mu.Lock()
		busy, path := w.loopRunning || w.running, w.statePath
		w.mu.Unlock()
		if busy {
			return errors.New("cannot reload a running UID watcher")
		}
		snapshot, err = readWatcherManagementState(path)
		if err != nil {
			return err
		}
	}

	// Management admission is closed and the background owner is paused. Recheck
	// local workers before publishing any of the staged snapshots.
	if a != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
	}
	if w != nil {
		w.mu.Lock()
		defer w.mu.Unlock()
	}
	if a != nil && (a.running || a.fetchLease || a.clientPreparing) {
		return errors.New("auto-enrollment started during management activation")
	}
	if w != nil && (w.loopRunning || w.running) {
		return errors.New("UID watcher started during management activation")
	}
	h.growthLedger = ledger
	if a != nil {
		a.held, a.blacklistPending = held, blacklists
	}
	if w != nil {
		w.state = snapshot
	}
	h.webAuthority.managementActive = true
	return nil
}

func (h *Handler) reloadGrowthLedger() (*growthLedger, error) {
	l := &growthLedger{path: h.cfg.GrowthLedgerPath, store: h.cfg.GrowthLedgerStore, byUID: map[string]*ledgerAccount{}}
	if l.store != nil {
		claims, err := l.store.LoadGrowthClaims()
		if err != nil {
			return nil, errors.New("cannot reload authoritative growth ledger")
		}
		for _, c := range claims {
			if c.UID != "" && c.TaskCode != "" {
				l.applyClaim(c.UID, c.TaskCode, c.Nickname, c.Credit, c.Energy, unixToRFC3339(c.ClaimedAtUnix))
			}
		}
		return l, nil
	}
	if l.path == "" {
		return l, nil
	}
	raw, err := os.ReadFile(l.path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, errors.New("cannot reload growth ledger file")
	}
	var stored struct {
		Accounts []*ledgerAccount `json:"accounts"`
	}
	if err = json.Unmarshal(raw, &stored); err != nil {
		return nil, errors.New("invalid growth ledger file")
	}
	l.loadJSON(raw)
	return l, nil
}

func readHeldManagementLedger(path string) (map[string]bool, map[string]string, error) {
	held, blacklists := map[string]bool{}, map[string]string{}
	if path == "" {
		return held, blacklists, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return held, blacklists, nil
	}
	if err != nil {
		return nil, nil, errors.New("cannot reload auto-enrollment ledger")
	}
	var phones []string
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		var ledger heldLedger
		if json.Unmarshal(raw, &ledger) != nil || ledger.Version != 2 || ledger.Phones == nil || ledger.Blacklists == nil {
			return nil, nil, errors.New("invalid auto-enrollment ledger")
		}
		phones, blacklists = ledger.Phones, ledger.Blacklists
		for phone, sid := range blacklists {
			if sid == "" || !slices.Contains(phones, phone) {
				return nil, nil, errors.New("invalid pending blacklist action")
			}
		}
	} else if json.Unmarshal(raw, &phones) != nil {
		return nil, nil, errors.New("invalid auto-enrollment ledger")
	}
	for _, phone := range phones {
		if phone != "" {
			held[phone] = true
		}
	}
	return held, blacklists, nil
}

func readWatcherManagementState(path string) (watcherState, error) {
	s := watcherState{PerUID: map[string]*watchUIDState{}}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, errors.New("cannot reload UID watcher state")
	}
	if json.Unmarshal(raw, &s) != nil || s.PerUID == nil {
		return s, errors.New("invalid UID watcher state")
	}
	for _, entry := range s.PerUID {
		if entry == nil {
			return s, errors.New("invalid UID watcher entry")
		}
	}
	return s, nil
}
