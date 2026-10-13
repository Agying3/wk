package session

import (
	"strings"
	"testing"
	"time"
)

func TestLeaseKeepsBusyHealthyPrimary(t *testing.T) {
	r := New(Config{Available: func() []string { return []string{"B"} }, Healthy: func() []string { return []string{"A", "B"} }})
	r.Bind("conversation", "A")
	e, ok := r.ResolveLease("conversation", nil, "")
	if !ok || e.UID != "A" {
		t.Fatalf("busy primary changed: %+v", e)
	}
	scoped, _ := r.ResolveLeaseWithCandidates("conversation", map[string]bool{"B": true}, "B", []string{"A", "B"}, []string{"B"})
	if scoped.UID != "B" {
		t.Fatal("out-of-scope primary was reused")
	}
}

func TestLeaseRejectsLateCompletionAndABA(t *testing.T) {
	store := newCountingStore()
	r := routerWith(store, []string{"A", "B"}, time.Minute)
	old, _ := r.ResolveLease("c", nil, "A")
	next, ok := r.ReplaceIfCurrent("c", old, "B")
	if !ok {
		t.Fatal("migration failed")
	}
	if r.TouchIfCurrent("c", old) || r.UnbindIfCurrent("c", old) {
		t.Fatal("old request mutated new binding")
	}
	back, ok := r.ReplaceIfCurrent("c", next, "A")
	if !ok || back.Version == old.Version {
		t.Fatal("version reused")
	}
	if _, ok := r.ReplaceIfCurrent("c", old, "B"); ok {
		t.Fatal("ABA admitted stale request")
	}
	if uid := store.LoadBinds()[r.normalizeKey("c")]; uid != "A" {
		t.Fatalf("mirror owner=%s", uid)
	}
}

func TestLeaseDoesNotReviveRemovedOrExpiredBinding(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"A"}, time.Minute)
	old, _ := r.ResolveLease("c", nil, "A")
	r.Unbind("c")
	if r.TouchIfCurrent("c", old) {
		t.Fatal("late completion revived deleted binding")
	}
	newer, _ := r.ResolveLease("c", nil, "A")
	r.mu.Lock()
	e := r.entries[newer.Hash]
	e.lastActive = time.Now().Add(-2 * time.Minute)
	r.entries[newer.Hash] = e
	r.mu.Unlock()
	if r.TouchIfCurrent("c", newer) {
		t.Fatal("late completion revived expired binding")
	}
}

func TestLongSessionIDsDoNotCollideAfter512Bytes(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"A", "B"}, time.Minute)
	a := strings.Repeat("x", 512) + "A"
	b := strings.Repeat("x", 512) + "B"
	x, _ := r.ResolveLease(a, nil, "A")
	y, _ := r.ResolveLease(b, nil, "B")
	if x.Hash == y.Hash || x.UID == y.UID {
		t.Fatal("identities with different suffixes collided")
	}
}
