package pool

import (
	"reflect"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestPlaygroundSnapshotNeverEntersSharedTransaction(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "snapshot-account", AccessToken: "fixture", ExpiresAt: 9999999999}
	p.Add(a)
	before := a.Snapshot()
	// A shared pool without a store is deliberately unusable for transactions.
	// Pure snapshot access must not enter begin/reap/commit at all.
	s := &sharedPool{state: sharedState{Version: 1, NextFence: 99}}
	p.shared.Store(s)
	defer p.shared.Store(nil)
	statuses, states := p.PlaygroundSnapshot()
	if len(statuses) != 1 || len(states) != 1 || statuses[0].UID != a.UID || !states[0].Healthy {
		t.Fatalf("snapshot=%+v %+v", statuses, states)
	}
	if s.tx != nil || s.cancel != nil || s.state.NextFence != 99 || s.fault.Load() != nil {
		t.Fatal("snapshot changed shared authority state")
	}
	if !reflect.DeepEqual(before, a.Snapshot()) {
		t.Fatal("snapshot generated device identity or changed credentials")
	}
}
