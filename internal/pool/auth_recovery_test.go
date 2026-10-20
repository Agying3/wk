package pool

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestAuthRecoverySingleFlightAndStrictQuarantine(t *testing.T) {
	a := &auth.Auth{UID: "bad"}
	p := modelTestPool(a)
	var starts atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if _, ok := p.BeginAuthRecovery(a, time.Minute); ok {
				starts.Add(1)
			}
		})
	}
	wg.Wait()
	if starts.Load() != 1 {
		t.Fatalf("refresh elections=%d, want 1", starts.Load())
	}
	p.SetCredits(a.UID, 2000)
	p.NoteSuccess(a.UID) // A pre-existing request completing cannot release auth isolation.
	st, _ := p.Status(a.UID)
	if !st.Cooling || st.CoolKind != "auth" || st.ErrTotal != 1 {
		t.Fatalf("auth isolation lost after credit/success update: %+v", st)
	}
	if p.Pick() != nil || p.PickByUID(a.UID) != nil || p.ServableNow() {
		t.Fatal("quarantined account was selected, including the all-cooling fallback")
	}
	_, healthy, cooling, disabled, _ := p.CountsDetailed()
	if healthy != 0 || cooling != 1 || disabled != 0 {
		t.Fatalf("counts healthy=%d cooling=%d disabled=%d", healthy, cooling, disabled)
	}
}

func TestAuthRecoverySurvivesBothPersistenceFormats(t *testing.T) {
	a := &auth.Auth{UID: "u"}
	p := modelTestPool(a)
	id, _ := p.BeginAuthRecovery(a, time.Hour)
	p.FinishAuthRecovery(a, id, "")
	for _, db := range []bool{false, true} {
		p.mu.RLock()
		saved := p.stateOverviewLocked().Accounts[a.UID]
		if db {
			saved = p.stateAccountOfLocked(p.byUID[a.UID])
		}
		raw, err := json.Marshal(saved)
		p.mu.RUnlock()
		if err != nil {
			t.Fatal(err)
		}
		var restored stateAccount
		if err := json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		q := New("")
		q.applyAccountsLocked(map[string]stateAccount{a.UID: restored})
		q.Add(a)
		if q.Pick() != nil {
			t.Fatalf("db=%t: restart released auth quarantine", db)
		}
		if _, ok := q.BeginAuthRecovery(a, time.Minute); ok {
			t.Fatalf("db=%t: restart started another refresh inside retry window", db)
		}
	}
}

func TestAuthRecoveryCannotDisableReplacementOrManualReset(t *testing.T) {
	for _, reset := range []string{"replace", "enable", "clear"} {
		t.Run(reset, func(t *testing.T) {
			a := &auth.Auth{UID: "u"}
			p := modelTestPool(a)
			id, _ := p.BeginAuthRecovery(a, time.Minute)
			switch reset {
			case "replace":
				p.Add(&auth.Auth{UID: a.UID, AccessToken: "new-login"})
			case "enable":
				p.Enable(a.UID)
			case "clear":
				p.ClearCooldown(a.UID)
			}
			if p.FinishAuthRecovery(a, id, "old refresh rejected") {
				t.Fatal("stale recovery was applied")
			}
			st, _ := p.Status(a.UID)
			if st.Disabled {
				t.Fatal("stale recovery disabled a manually repaired account")
			}
			p.mu.Lock()
			p.byUID[a.UID].authUntil = time.Now().Add(-time.Second)
			p.mu.Unlock()
			if p.Pick() == nil {
				t.Fatal("replacement remained stuck in recovering state")
			}
		})
	}
}
