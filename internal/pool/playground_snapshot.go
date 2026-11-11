package pool

import (
	"sort"
	"time"
)

// PlaygroundSnapshot returns this process's last observed state without taking
// the shared authority lock, reaping leases, or committing a new revision.
// It is advisory UI metadata; inference must still use normal pool admission.
func (p *Pool) PlaygroundSnapshot() ([]Status, []RoutingState) {
	p.mu.local.Lock()
	defer p.mu.local.Unlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	statuses := make([]Status, 0, len(uids))
	states := make([]RoutingState, 0, len(uids))
	now := time.Now()
	for _, uid := range uids {
		e := p.byUID[uid]
		statuses = append(statuses, p.statusOf(uid, e))
		states = append(states, RoutingState{UID: uid, Healthy: e.healthyForModel("", now), InFlight: int(e.inFlight.Load()), Limit: p.maxInFlight, Waiting: len(p.waiters[uid])})
	}
	return statuses, states
}
