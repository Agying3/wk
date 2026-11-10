package collection

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPausedRelayDoesNotRecoverOrOverwriteLiveOwner(t *testing.T) {
	store := &memoryStore{rows: map[string][]byte{
		"job/live":          encode(Job{ID: "live", Status: "running"}),
		"item/live/u/usage": encode(Item{Target: Target{"u", "usage"}, Status: "running"}),
	}}
	m, err := New(Options{Store: store, StartPaused: true, Run: func(context.Context, Target, Snapshot) (json.RawMessage, error) {
		t.Error("relay ran collection")
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if j, _ := m.Job("live"); j.Status != "running" {
		t.Fatal("relay interrupted owner job")
	}
	if _, err = m.Submit(Request{UIDs: []string{"u"}, Resources: []string{"usage"}}); err == nil {
		t.Fatal("relay accepted a job")
	}
	// Owner finishes after relay construction. Relay shutdown/flush must not
	// restore its old running snapshot, and promotion must reload this result.
	if err = store.CollectionSave(map[string][]byte{"job/live": encode(Job{ID: "live", Status: "completed"})}, nil); err != nil {
		t.Fatal(err)
	}
	if err = m.FlushChecked(); err != nil {
		t.Fatal(err)
	}
	if err = m.ActivateBackground(); err != nil {
		t.Fatal(err)
	}
	if j, _ := m.Job("live"); j.Status != "completed" {
		t.Fatalf("promotion overwrote owner result: %s", j.Status)
	}
}

func TestPausedRelayRecoversOnlyAfterPromotion(t *testing.T) {
	store := &memoryStore{rows: map[string][]byte{"job/live": encode(Job{ID: "live", Status: "running"})}}
	m, err := New(Options{Store: store, StartPaused: true, Run: func(context.Context, Target, Snapshot) (json.RawMessage, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err = m.ActivateBackground(); err != nil {
		t.Fatal(err)
	}
	if j, _ := m.Job("live"); j.Status != "interrupted" {
		t.Fatalf("promotion did not recover: %s", j.Status)
	}
}
