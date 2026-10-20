package pool

import (
	"time"

	"workbuddy2api/internal/auth"
)

// BeginAuthRecovery atomically quarantines an account and elects one refresher.
// The independent deadline survives restarts, credit updates and old successes.
func (p *Pool) BeginAuthRecovery(a *auth.Auth, cooldown time.Duration) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[a.UID]
	if !ok || e.a != a || e.disabled || e.authRecovering || time.Now().Before(e.authUntil) {
		return 0, false
	}
	e.authUntil = time.Now().Add(cooldown)
	e.authRecoveryID++
	e.authRecovering = true
	e.errTotal++
	e.lastErr = time.Now()
	p.noteChangeLocked(a.UID)
	return e.authRecoveryID, true
}

// FinishAuthRecovery never alters a replacement credential or a manual reset.
// Even a successful refresh retains the short quarantine so late 401s cannot
// start another refresh wave. Actual chat success is checked after its expiry.
func (p *Pool) FinishAuthRecovery(a *auth.Auth, id uint64, disableReason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[a.UID]
	if !ok || e.a != a || e.authRecoveryID != id || !e.authRecovering {
		return false
	}
	e.authRecovering = false
	if disableReason != "" && !e.disabled {
		e.disabled = true
		e.reason = disableReason
	}
	p.noteChangeLocked(a.UID)
	return true
}
