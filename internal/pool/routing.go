package pool

import (
	"context"
	"errors"
	"sort"
	"time"

	"workbuddy2api/internal/auth"
)

var ErrAccountUnavailable = errors.New("account unavailable for model")
var ErrWaitQueueFull = errors.New("account wait queue full")
var ErrRouteReserved = errors.New("account reserved for active sessions")
var ErrSessionQueueFull = errors.New("session wait queue full")

type slotWaiter struct {
	notify   chan struct{}
	route    RouteRequest
	enqueued time.Time
}

type queueTurn struct {
	tenant   string
	sessions map[string]string
	hotBurst int
}

// RoutingState separates health from instantaneous capacity. Observations are
// advisory; reservation must still happen under the pool lock.
type RoutingState struct {
	UID            string
	Healthy        bool
	InFlight       int
	Limit          int
	Waiting        int
	ActiveSessions int
	HotSessions    int
}

func (p *Pool) HealthyUIDs() []string { return p.HealthyUIDsForModel("") }
func (p *Pool) HealthyForModel(uid, model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.byUID[uid]
	return e != nil && e.healthyForModel(normalizeModel(model), time.Now())
}
func (p *Pool) HealthyUIDsForModel(model string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var out []string
	for uid, e := range p.byUID {
		if e.healthyForModel(normalizeModel(model), now) {
			out = append(out, uid)
		}
	}
	sort.Strings(out)
	return out
}

func (p *Pool) RoutingStates(model string, allow map[string]bool) []RoutingState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	out := make([]RoutingState, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if allow != nil && !allow[uid] {
			continue
		}
		active, hot := p.routePressureLocked(uid, now)
		out = append(out, RoutingState{UID: uid, Healthy: e.healthyForModel(normalizeModel(model), now), InFlight: int(e.inFlight.Load()), Limit: p.maxInFlight, Waiting: len(p.waiters[uid]), ActiveSessions: active, HotSessions: hot})
	}
	return out
}

// Tenant rotation prevents multiple session IDs from buying more queue turns.
// Within each tenant, sessions rotate. Aging overrides cache priority and
// rotation so cold/older requests cannot be indefinitely overtaken.
func (p *Pool) nextWaiterLocked(uid string, now time.Time) *slotWaiter {
	q := p.waiters[uid]
	if len(q) == 0 {
		return nil
	}
	if !p.admission.FairQueue || q[0].route.Tenant == "" {
		return q[0]
	}
	for _, w := range q {
		if now.Sub(w.enqueued) >= p.admission.QueueAging {
			return w
		}
	}
	turn := p.queueTurns[uid]
	if turn == nil {
		turn = &queueTurn{sessions: map[string]string{}}
		p.queueTurns[uid] = turn
	}
	var candidates []*slotWaiter
	for _, w := range q {
		if w.route.Tenant != turn.tenant {
			candidates = append(candidates, w)
		}
	}
	if len(candidates) == 0 {
		candidates = q
	}
	tenant := candidates[0].route.Tenant
	var sessions []*slotWaiter
	for _, w := range candidates {
		if w.route.Tenant == tenant {
			sessions = append(sessions, w)
		}
	}
	var rotated []*slotWaiter
	for _, w := range sessions {
		if w.route.Session != turn.sessions[tenant] {
			rotated = append(rotated, w)
		}
	}
	if len(rotated) > 0 {
		sessions = rotated
	}
	preferHot := turn.hotBurst < 2
	for _, w := range sessions {
		if p.hotRouteLocked(uid, w.route, now) == preferHot {
			return w
		}
	}
	return sessions[0]
}

func (p *Pool) notifyWaiterLocked(uid string) {
	if w := p.nextWaiterLocked(uid, time.Now()); w != nil {
		select {
		case w.notify <- struct{}{}:
		default:
		}
	}
}

func (p *Pool) removeWaiterLocked(uid string, w *slotWaiter) {
	q := p.waiters[uid]
	found := false
	for i, v := range q {
		if v == w {
			q = append(q[:i], q[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return
	}
	p.totalWaiters--
	if len(q) == 0 {
		delete(p.waiters, uid)
		delete(p.queueTurns, uid)
	} else {
		p.waiters[uid] = q
		if turn := p.queueTurns[uid]; turn != nil {
			present := map[string]bool{}
			for _, pending := range q {
				present[pending.route.Tenant] = true
			}
			for tenant := range turn.sessions {
				if !present[tenant] {
					delete(turn.sessions, tenant)
				}
			}
		}
		p.notifyWaiterLocked(uid)
	}
}

// WaitAndAcquireByUIDForModel is the identity-free FIFO compatibility path.
// Route-aware callers use tenant/session rotation and aging when configured.
// Ordinary selectors cannot jump either queue; cancellation removes waiters.
func (p *Pool) WaitAndAcquireByUIDForModel(ctx context.Context, uid, model string, maxPending, maxTotal int) (*auth.Auth, error) {
	return p.WaitAndAcquireByUIDForRoute(ctx, uid, model, maxPending, maxTotal, RouteRequest{}, nil, nil)
}

func (p *Pool) WaitAndAcquireByUIDForRoute(ctx context.Context, uid, model string, maxPending, maxTotal int, route RouteRequest, tried, allow map[string]bool) (*auth.Auth, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxPending <= 0 || maxTotal <= 0 {
		return nil, ErrWaitQueueFull
	}
	model = normalizeModel(model)
	w := &slotWaiter{notify: make(chan struct{}, 1), route: route, enqueued: time.Now()}
	p.mu.Lock()
	e := p.byUID[uid]
	if e == nil || !e.healthyForModel(model, time.Now()) {
		p.mu.Unlock()
		return nil, ErrAccountUnavailable
	}
	if (allow != nil && !allow[uid]) || tried[uid] {
		p.mu.Unlock()
		return nil, ErrAccountUnavailable
	}
	if p.reservedLocked(uid, route, time.Now()) && p.alternativeLocked(uid, model, route, tried, allow, time.Now()) {
		p.mu.Unlock()
		return nil, ErrRouteReserved
	}
	if len(p.waiters[uid]) >= maxPending || p.totalWaiters >= maxTotal {
		p.mu.Unlock()
		return nil, ErrWaitQueueFull
	}
	if p.admission.FairQueue && route.Session != "" {
		pending := 0
		for _, other := range p.waiters[uid] {
			if other.route.Tenant == route.Tenant && other.route.Session == route.Session {
				pending++
			}
		}
		if pending >= min(maxPending, p.admission.MaxPendingPerSession) {
			p.mu.Unlock()
			return nil, ErrSessionQueueFull
		}
	}
	if p.waiters == nil {
		p.waiters = map[string][]*slotWaiter{}
	}
	p.waiters[uid] = append(p.waiters[uid], w)
	p.totalWaiters++
	p.mu.Unlock()
	// Health/config changes need not signal capacity; periodic recheck covers
	// those transitions without a permanent worker goroutine per account.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	defer func() { p.mu.Lock(); p.removeWaiterLocked(uid, w); p.mu.Unlock() }()
	for {
		p.mu.Lock()
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		e = p.byUID[uid]
		if e == nil || !e.healthyForModel(model, time.Now()) {
			p.mu.Unlock()
			return nil, ErrAccountUnavailable
		}
		if p.reservedLocked(uid, route, time.Now()) && p.alternativeLocked(uid, model, route, tried, allow, time.Now()) {
			p.mu.Unlock()
			return nil, ErrRouteReserved
		}
		if p.nextWaiterLocked(uid, time.Now()) == w && (p.maxInFlight <= 0 || e.inFlight.Load() < int64(p.maxInFlight)) {
			if turn := p.queueTurns[uid]; turn != nil {
				turn.tenant = route.Tenant
				turn.sessions[route.Tenant] = route.Session
				if p.hotRouteLocked(uid, route, time.Now()) {
					turn.hotBurst++
				} else {
					turn.hotBurst = 0
				}
			}
			p.markUsedLocked(e)
			e.inFlight.Add(1)
			p.startRouteLocked(uid, route, time.Now())
			a := e.a
			p.removeWaiterLocked(uid, w)
			p.mu.Unlock()
			return a, nil
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-w.notify:
		case <-ticker.C:
		}
	}
}
