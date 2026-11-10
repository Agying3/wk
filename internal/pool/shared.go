package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/coordination"
)

// poolMutex retains the existing pool state machine's critical sections. In
// overlap mode every section reads and commits the same PostgreSQL authority;
// readers are exclusive too, because they must observe one coherent revision.
type poolMutex struct {
	local sync.Mutex
	pool  *Pool
}

func (m *poolMutex) Lock() {
	m.local.Lock()
	if m.pool != nil {
		if s := m.pool.shared.Load(); s != nil {
			s.begin(m.pool)
		}
	}
}
func (m *poolMutex) Unlock() {
	if m.pool != nil {
		if s := m.pool.shared.Load(); s != nil {
			s.finish(m.pool)
		}
	}
	m.local.Unlock()
}
func (m *poolMutex) RLock()   { m.Lock() }
func (m *poolMutex) RUnlock() { m.Unlock() }

type sharedFailure struct{ err error }
type sharedPool struct {
	store  *coordination.Store
	owner  string
	alive  func(string) (bool, error)
	fault  atomic.Pointer[sharedFailure]
	tx     *coordination.Tx
	cancel context.CancelFunc
	state  sharedState
	before sharedState
	hidden map[string]*entry
}

// Lease is request-local. Only its owner may release it; the authoritative row
// also stores the route, so a caller cannot decrement another session's lease.
type Lease struct {
	ID, UID, Owner string
	Fence          uint64
}
type sharedLease struct {
	Lease
	Route  RouteRequest
	Routed bool
}
type sharedAccount struct {
	State stateAccount
	// CredentialPath is non-secret metadata. Credentials remain exclusively in
	// the shared auth directory, whose identity is validated before overlap.
	CredentialPath    string
	Spent             float64
	LastUsed          time.Time
	BreakerUntil      time.Time
	Fails, RetryCount int
	RecoveryID        uint64
	Recovering        bool
	RecoveryOwner     string
}
type sharedRoute struct {
	Tenant, Prefix       string
	Active               int
	Success              bool
	LastSuccess, Expires time.Time
	Gap                  time.Duration
}
type sharedWaiter struct {
	ID, Owner string
	Route     RouteRequest
	Enqueued  time.Time
}
type sharedTurn struct {
	Tenant   string
	Sessions map[string]string
	HotBurst int
}
type sharedPolicy struct {
	MaxInFlight                         int
	Admission                           AdmissionConfig
	BreakerThreshold, RiskThreshold     int
	BreakerCooldown, BreakerCooldownMax time.Duration
	IdlePerHour, IdleMax                float64
}
type sharedState struct {
	Version      int
	Accounts     map[string]sharedAccount
	Leases       map[string]sharedLease
	Routes       map[string]map[string]sharedRoute
	ServiceTimes map[string]time.Duration
	LastPickAt   time.Time
	NextFence    uint64
	Policy       sharedPolicy
	Waiters      map[string][]sharedWaiter
	Turns        map[string]sharedTurn
}

func (p *Pool) SharedEnabled() bool { return p.shared.Load() != nil }
func (p *Pool) SharedError() error {
	if s := p.shared.Load(); s != nil {
		if f := s.fault.Load(); f != nil {
			return f.err
		}
	}
	return nil
}

// EnableShared must run before serving. Only the sole initial management owner
// may bootstrap; later releases must never import their stale local snapshots.
func (p *Pool) EnableShared(ctx context.Context, store *coordination.Store, owner string, alive func(string) (bool, error), bootstrap ...bool) error {
	if store == nil || owner == "" || alive == nil {
		return errors.New("shared pool requires an authority, owner and local owner guard")
	}
	p.persistMu.Lock()
	defer p.persistMu.Unlock()
	p.mu.local.Lock()
	defer p.mu.local.Unlock()
	if p.SharedEnabled() {
		return errors.New("shared pool already enabled")
	}
	tx, err := store.Begin(ctx, "pool/runtime")
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s := &sharedPool{store: store, owner: owner, alive: alive}
	raw := tx.Value()
	if string(raw) == "null" {
		if len(bootstrap) == 0 || !bootstrap[0] {
			return errors.New("shared pool is not initialized by its management owner")
		}
		s.state = sharedState{Version: 1, Leases: map[string]sharedLease{}}
		s.state = s.capture(p)
	} else if err = json.Unmarshal(raw, &s.state); err != nil {
		return fmt.Errorf("decode shared pool: %w", err)
	}
	if s.state.Version != 1 || s.state.Accounts == nil || s.state.Leases == nil {
		return errors.New("incompatible shared pool state")
	}
	encoded, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	if err = tx.Commit(encoded); err != nil {
		return err
	}
	p.mu.pool = p
	p.persister = nil
	p.store = nil
	p.stateFp = ""
	p.dirty.Store(false)
	p.dirtySet = map[string]dirtyOp{}
	s.restore(p, s.state)
	p.shared.Store(s)
	return nil
}

// AttachSharedAuths binds credentials without adding/deleting shared accounts.
func (p *Pool) AttachSharedAuths(accounts []*auth.Auth) error {
	p.mu.Lock()
	for _, a := range accounts {
		if e := p.byUID[a.UID]; e != nil {
			e.a = a
		}
	}
	p.mu.Unlock()
	return p.SharedError()
}

func (s *sharedPool) fail(err error) {
	if err != nil {
		s.fault.CompareAndSwap(nil, &sharedFailure{fmt.Errorf("shared pool authority unavailable: %w", err)})
	}
}

func (s *sharedPool) begin(p *Pool) {
	// An ambiguous commit is a permanent fence for this process. It must not
	// guess whether a state mutation succeeded and overwrite a newer revision.
	if s.fault.Load() != nil {
		s.hidden = p.byUID
		p.byUID = map[string]*entry{}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	s.cancel = cancel
	tx, err := s.store.Begin(ctx, "pool/runtime")
	if err != nil {
		cancel()
		s.cancel = nil
		s.fail(err)
		s.hidden = p.byUID
		p.byUID = map[string]*entry{}
		return
	}
	var state sharedState
	if err = json.Unmarshal(tx.Value(), &state); err == nil && (state.Version != 1 || state.Accounts == nil || state.Leases == nil) {
		err = errors.New("incompatible shared pool state")
	}
	if err == nil {
		err = s.reap(&state)
	}
	if err != nil {
		tx.Rollback()
		cancel()
		s.cancel = nil
		s.fail(err)
		s.hidden = p.byUID
		p.byUID = map[string]*entry{}
		return
	}
	s.tx = tx
	s.state = state
	// Keep a distinct rollback snapshot: leases and queue records are mutated in place.
	if err = json.Unmarshal(tx.Value(), &s.before); err != nil {
		tx.Rollback()
		cancel()
		s.tx = nil
		s.cancel = nil
		s.fail(err)
		s.hidden = p.byUID
		p.byUID = map[string]*entry{}
		return
	}
	s.restore(p, state)
}

func (s *sharedPool) finish(p *Pool) {
	if s.hidden != nil {
		p.byUID = s.hidden
		s.hidden = nil
		return
	}
	if s.tx == nil {
		return
	}
	tx := s.tx
	s.tx = nil
	next := s.capture(p)
	raw, err := json.Marshal(next)
	if err == nil {
		err = tx.Commit(raw)
	} else {
		tx.Rollback()
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if err != nil {
		s.fail(err)
		s.restore(p, s.before)
		return
	}
	s.state = next
	p.dirty.Store(false)
	p.dirtySet = map[string]dirtyOp{}
}

func (s *sharedPool) capture(p *Pool) sharedState {
	r := s.state
	r.Version = 1
	r.Accounts = make(map[string]sharedAccount, len(p.byUID))
	r.Routes = map[string]map[string]sharedRoute{}
	if r.Leases == nil {
		r.Leases = map[string]sharedLease{}
	}
	for uid, e := range p.byUID {
		owner := ""
		if e.authRecovering {
			owner = s.state.Accounts[uid].RecoveryOwner
			if owner == "" {
				owner = s.owner
			}
		}
		path := e.a.Snapshot().FilePath
		// A candidate that cannot yet read a newly enrolled file must not erase
		// the owner's metadata. AttachSharedAuths can fill legacy empty paths.
		if path == "" {
			path = s.state.Accounts[uid].CredentialPath
		}
		r.Accounts[uid] = sharedAccount{State: p.stateAccountOfLocked(e), CredentialPath: path, Spent: e.spent, LastUsed: e.lastUsed, BreakerUntil: e.breakerUntil, Fails: e.fails, RetryCount: e.retryCount, RecoveryID: e.authRecoveryID, Recovering: e.authRecovering, RecoveryOwner: owner}
	}
	for uid, routes := range p.routeLeases {
		r.Routes[uid] = map[string]sharedRoute{}
		for key, l := range routes {
			r.Routes[uid][key] = sharedRoute{l.tenant, l.prefix, l.active, l.success, l.lastSuccess, l.expires, l.gap}
		}
	}
	r.ServiceTimes = p.serviceTimes
	r.LastPickAt = p.lastPickAt
	r.Waiters = map[string][]sharedWaiter{}
	r.Turns = map[string]sharedTurn{}
	for uid, q := range p.waiters {
		for _, w := range q {
			r.Waiters[uid] = append(r.Waiters[uid], sharedWaiter{w.id, w.owner, w.route, w.enqueued})
		}
	}
	for uid, t := range p.queueTurns {
		r.Turns[uid] = sharedTurn{t.tenant, t.sessions, t.hotBurst}
	}
	r.Policy = sharedPolicy{p.maxInFlight, p.admission, p.breakerThreshold, p.riskDisableThreshold, p.breakerCooldown, p.breakerCooldownMax, p.idleWeightPerHour, p.idleWeightMax}
	return r
}

func (s *sharedPool) restore(p *Pool, r sharedState) {
	next := map[string]*entry{}
	for uid, account := range r.Accounts {
		a := &auth.Auth{UID: uid}
		if old := p.byUID[uid]; old != nil {
			a = old.a
		}
		path := a.Snapshot().FilePath
		if account.CredentialPath != "" && (path == "" || path != account.CredentialPath) {
			// This is a read-only snapshot, never a refresh/save under the pool
			// authority lock. A missing, malformed, or mismatched file disables
			// only this local account until a subsequent restore can load it.
			a = &auth.Auth{UID: uid}
			if raw, err := os.ReadFile(account.CredentialPath); err == nil {
				if loaded, err := auth.Parse(raw); err == nil && loaded.UID == uid {
					loaded.FilePath = account.CredentialPath
					a = loaded
				}
			}
		}
		v := account.State
		e := &entry{a: a, credits: v.Credits, capacitySize: v.CapacitySize, capacityRemain: v.CapacityRemain, capacityUsed: v.CapacityUsed, cycleCapacitySize: v.CycleCapacitySize, cycleCapacityRemain: v.CycleCapacityRemain, cycleCapacityUsed: v.CycleCapacityUsed, creditUpdatedAt: v.CreditUpdatedAt, disabled: v.Disabled, reason: v.Reason, until: v.Until, coolKind: v.CoolKind, modelCooldowns: loadModelCooldowns(v.ModelCooldowns), successCount: v.SuccessCount, errTotal: v.ErrTotal, lastErr: v.LastErr, lastSuccess: v.LastSuccess, riskStrikes: v.RiskStrikes, authUntil: v.AuthUntil, spent: account.Spent, lastUsed: account.LastUsed, breakerUntil: account.BreakerUntil, fails: account.Fails, retryCount: account.RetryCount, authRecoveryID: account.RecoveryID, authRecovering: account.Recovering}
		next[uid] = e
	}
	for _, lease := range r.Leases {
		if e := next[lease.UID]; e != nil {
			e.inFlight.Add(1)
		}
	}
	p.byUID = next
	p.routeLeases = map[string]map[string]*routeLease{}
	p.leaseCount = 0
	for uid, routes := range r.Routes {
		p.routeLeases[uid] = map[string]*routeLease{}
		for key, l := range routes {
			p.routeLeases[uid][key] = &routeLease{l.Tenant, l.Prefix, l.Active, l.Success, l.LastSuccess, l.Expires, l.Gap}
			p.leaseCount++
		}
	}
	p.serviceTimes = r.ServiceTimes
	if p.serviceTimes == nil {
		p.serviceTimes = map[string]time.Duration{}
	}
	p.lastPickAt = r.LastPickAt
	p.waiters = map[string][]*slotWaiter{}
	p.queueTurns = map[string]*queueTurn{}
	p.totalWaiters = 0
	for uid, q := range r.Waiters {
		for _, w := range q {
			p.waiters[uid] = append(p.waiters[uid], &slotWaiter{id: w.ID, owner: w.Owner, route: w.Route, enqueued: w.Enqueued})
			p.totalWaiters++
		}
	}
	for uid, t := range r.Turns {
		p.queueTurns[uid] = &queueTurn{t.Tenant, t.Sessions, t.HotBurst}
	}
	p.maxInFlight = r.Policy.MaxInFlight
	p.admission = r.Policy.Admission
	p.breakerThreshold = r.Policy.BreakerThreshold
	p.riskDisableThreshold = r.Policy.RiskThreshold
	p.breakerCooldown = r.Policy.BreakerCooldown
	p.breakerCooldownMax = r.Policy.BreakerCooldownMax
	p.idleWeightPerHour = r.Policy.IdlePerHour
	p.idleWeightMax = r.Policy.IdleMax
}

func (s *sharedPool) reap(r *sharedState) error {
	owners := map[string]bool{}
	for _, lease := range r.Leases {
		owners[lease.Owner] = false
	}
	for _, a := range r.Accounts {
		if a.Recovering {
			owners[a.RecoveryOwner] = false
		}
	}
	for _, q := range r.Waiters {
		for _, w := range q {
			owners[w.Owner] = false
		}
	}
	for owner := range owners {
		alive, err := s.alive(owner)
		if err != nil {
			return err
		}
		owners[owner] = alive
	}
	for id, l := range r.Leases {
		if !owners[l.Owner] {
			delete(r.Leases, id)
			if l.Routed {
				if route, ok := r.Routes[l.UID][routeLeaseKey(l.Route)]; ok && route.Active > 0 {
					route.Active--
					r.Routes[l.UID][routeLeaseKey(l.Route)] = route
				}
			}
		}
	}
	for uid, a := range r.Accounts {
		if a.Recovering && !owners[a.RecoveryOwner] {
			a.Recovering = false
			a.RecoveryID++
			a.RecoveryOwner = ""
			r.Accounts[uid] = a
		}
	}
	for uid, q := range r.Waiters {
		keep := q[:0]
		for _, w := range q {
			if owners[w.Owner] {
				keep = append(keep, w)
			}
		}
		r.Waiters[uid] = keep
		if len(keep) == 0 {
			delete(r.Turns, uid)
		}
	}
	return nil
}

// AcquireShared selects and reserves with one authority transaction. It never
// waits for a release-wide drain and never returns an uncommitted reservation.
func (p *Pool) AcquireShared(model string, r RouteRequest, tried, allow map[string]bool, preferredUID string, useRoute bool) (*auth.Auth, Lease, error) {
	if !p.SharedEnabled() {
		return nil, Lease{}, errors.New("shared pool is not enabled")
	}
	p.mu.Lock()
	s := p.shared.Load()
	if err := p.SharedError(); err != nil {
		p.mu.Unlock()
		return nil, Lease{}, err
	}
	model = normalizeModel(model)
	// A candidate may not yet have observed an account added by the owner.
	excluded := make(map[string]bool, len(tried))
	for k, v := range tried {
		excluded[k] = v
	}
	for uid, e := range p.byUID {
		if e.a.Snapshot().FilePath == "" {
			excluded[uid] = true
		}
	}
	var a *auth.Auth
	if preferredUID != "" {
		if useRoute {
			now := time.Now()
			if p.readyRouteLocked(preferredUID, model, excluded, allow, now) && !(p.reservedLocked(preferredUID, r, now) && p.alternativeLocked(preferredUID, model, r, excluded, allow, now)) {
				a = p.byUID[preferredUID].a
				p.markUsedLocked(p.byUID[preferredUID])
			}
		} else if !excluded[preferredUID] && (allow == nil || allow[preferredUID]) {
			a = p.pickByUIDForModelLocked(preferredUID, model)
		}
	} else if useRoute {
		uid := p.recommendRouteLocked(model, r, excluded, allow, time.Now())
		if uid != "" {
			a = p.byUID[uid].a
			p.markUsedLocked(p.byUID[uid])
		} else if allow == nil {
			a = p.pickForModelLocked(model, excluded)
		} else {
			a = p.pickForModelAllowLocked(model, excluded, allow)
		}
	} else if allow == nil {
		a = p.pickForModelLocked(model, excluded)
	} else {
		a = p.pickForModelAllowLocked(model, excluded, allow)
	}
	var lease Lease
	if a != nil {
		var nonce [24]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			p.mu.Unlock()
			return nil, Lease{}, err
		}
		s.state.NextFence++
		lease = Lease{hex.EncodeToString(nonce[:]), a.UID, s.owner, s.state.NextFence}
		s.state.Leases[lease.ID] = sharedLease{Lease: lease, Route: r, Routed: useRoute}
		p.byUID[a.UID].inFlight.Add(1)
		if useRoute {
			p.startRouteLocked(a.UID, r, time.Now())
		}
	}
	p.mu.Unlock()
	if err := p.SharedError(); err != nil {
		return nil, Lease{}, err
	}
	return a, lease, nil
}

func (p *Pool) ReleaseLease(lease Lease) error {
	if !p.SharedEnabled() {
		return errors.New("shared pool is not enabled")
	}
	p.mu.Lock()
	s := p.shared.Load()
	if err := p.SharedError(); err != nil {
		p.mu.Unlock()
		return err
	}
	l, exists := s.state.Leases[lease.ID]
	if exists {
		if l.Lease != lease || lease.Owner != s.owner {
			p.mu.Unlock()
			return errors.New("lease ownership mismatch")
		}
		delete(s.state.Leases, lease.ID)
		if l.Routed {
			if route := p.routeLeases[l.UID][routeLeaseKey(l.Route)]; route != nil && route.active > 0 {
				route.active--
			}
		}
		if e := p.byUID[l.UID]; e != nil && e.inFlight.Load() > 0 {
			e.inFlight.Add(-1)
			p.notifyWaiterLocked(l.UID)
		}
	}
	p.mu.Unlock()
	return p.SharedError()
}

// WaitAcquireShared retains the normal bounded, fair account queue across
// overlapping processes. It is unrelated to release draining; ready accounts
// continue accepting work throughout a deployment.
func (p *Pool) WaitAcquireShared(ctx context.Context, uid, model string, maxPending, maxTotal int, route RouteRequest, tried, allow map[string]bool) (*auth.Auth, Lease, error) {
	if !p.SharedEnabled() {
		return nil, Lease{}, errors.New("shared pool is not enabled")
	}
	if maxPending <= 0 || maxTotal <= 0 {
		return nil, Lease{}, ErrWaitQueueFull
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, Lease{}, err
	}
	id := hex.EncodeToString(nonce[:])
	model = normalizeModel(model)
	registered := false
	defer func() {
		if registered {
			p.mu.Lock()
			for _, w := range p.waiters[uid] {
				if w.id == id {
					p.removeWaiterLocked(uid, w)
					break
				}
			}
			p.mu.Unlock()
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, Lease{}, err
		}
		p.mu.Lock()
		s := p.shared.Load()
		if err := p.SharedError(); err != nil {
			p.mu.Unlock()
			return nil, Lease{}, err
		}
		e := p.byUID[uid]
		now := time.Now()
		if e == nil || e.a.Snapshot().FilePath == "" || !e.healthyForModel(model, now) || tried[uid] || (allow != nil && !allow[uid]) {
			p.mu.Unlock()
			return nil, Lease{}, ErrAccountUnavailable
		}
		if p.reservedLocked(uid, route, now) && p.alternativeLocked(uid, model, route, tried, allow, now) {
			p.mu.Unlock()
			return nil, Lease{}, ErrRouteReserved
		}
		var waiter *slotWaiter
		if !registered {
			if len(p.waiters[uid]) >= maxPending || p.totalWaiters >= maxTotal {
				p.mu.Unlock()
				return nil, Lease{}, ErrWaitQueueFull
			}
			if p.admission.FairQueue && route.Session != "" {
				pending := 0
				for _, w := range p.waiters[uid] {
					if w.route.Tenant == route.Tenant && w.route.Session == route.Session {
						pending++
					}
				}
				if pending >= min(maxPending, p.admission.MaxPendingPerSession) {
					p.mu.Unlock()
					return nil, Lease{}, ErrSessionQueueFull
				}
			}
			waiter = &slotWaiter{id: id, owner: s.owner, route: route, enqueued: now}
			p.waiters[uid] = append(p.waiters[uid], waiter)
			p.totalWaiters++
			registered = true
		} else {
			for _, w := range p.waiters[uid] {
				if w.id == id {
					waiter = w
					break
				}
			}
			if waiter == nil {
				p.mu.Unlock()
				return nil, Lease{}, errors.New("shared queue reservation disappeared")
			}
		}
		if p.nextWaiterLocked(uid, now) == waiter && (p.maxInFlight <= 0 || e.inFlight.Load() < int64(p.maxInFlight)) {
			if turn := p.queueTurns[uid]; turn != nil {
				turn.tenant = route.Tenant
				turn.sessions[route.Tenant] = route.Session
				if p.hotRouteLocked(uid, route, now) {
					turn.hotBurst++
				} else {
					turn.hotBurst = 0
				}
			}
			p.markUsedLocked(e)
			e.inFlight.Add(1)
			p.startRouteLocked(uid, route, now)
			p.removeWaiterLocked(uid, waiter)
			registered = false
			s.state.NextFence++
			lease := Lease{id, uid, s.owner, s.state.NextFence}
			s.state.Leases[id] = sharedLease{lease, route, true}
			a := e.a
			p.mu.Unlock()
			if err := p.SharedError(); err != nil {
				return nil, Lease{}, err
			}
			if err := ctx.Err(); err != nil {
				_ = p.ReleaseLease(lease)
				return nil, Lease{}, err
			}
			return a, lease, nil
		}
		p.mu.Unlock()
		if err := p.SharedError(); err != nil {
			return nil, Lease{}, err
		}
		select {
		case <-ctx.Done():
			return nil, Lease{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
