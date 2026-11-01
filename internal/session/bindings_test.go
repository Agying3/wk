package session

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSharedBindingStoreSerializesFirstAllocation(t *testing.T) {
	s := NewBindingStore[string](BindingPolicy{TTL: time.Hour, MaxEntries: 10, RejectWhenFull: true}, nil)
	var wg sync.WaitGroup
	allocations := 0
	commits := 0
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Update("same", time.Now(), time.Hour, func(value string, live bool) (string, bool, error) {
				if live {
					if value != "first" {
						t.Error("binding changed")
					}
					return value, false, nil
				}
				allocations++
				return "first", true, nil
			}, func() error { commits++; return nil })
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if allocations != 1 || commits != 1 || len(s.entries) != 1 {
		t.Fatal("multiple first bindings", allocations, commits)
	}
}

func TestSharedBindingStoreRollbackAndAtomicLegacyMigration(t *testing.T) {
	now := time.Now()
	old := Binding[string]{Value: "selected", LastSeen: now, Version: 8}
	entries := map[string]Binding[string]{"legacy": old}
	s := NewBindingStore[string](BindingPolicy{TTL: 24 * time.Hour, MaxEntries: 1, RejectWhenFull: true}, entries)
	fail := errors.New("disk unavailable")
	keep := func(value string, live bool) (string, bool, error) {
		if !live || value != "selected" {
			t.Fatal("legacy choice was lost")
		}
		return value, false, nil
	}
	if err := s.UpdateAlias("unified", "legacy", now, time.Hour, keep, func() error { return fail }); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	if entries["legacy"] != old || len(entries) != 1 {
		t.Fatal("failed migration changed live binding")
	}
	if err := s.UpdateAlias("unified", "legacy", now, time.Hour, keep, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := entries["legacy"]; ok || len(entries) != 1 || entries["unified"].Value != "selected" {
		t.Fatal("migration consumed extra capacity or rerolled")
	}
	before := entries["unified"]
	if err := s.Update("unified", now.Add(2*time.Hour), time.Hour, keep, func() error { return fail }); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	if entries["unified"] != before {
		t.Fatal("failed renewal changed last-seen or generation")
	}
}

func TestSharedBindingStoreExpiryAndCapacityPolicies(t *testing.T) {
	now := time.Now()
	for _, reject := range []bool{true, false} {
		s := NewBindingStore[string](BindingPolicy{TTL: time.Hour, MaxEntries: 1, RejectWhenFull: reject}, nil)
		set := func(key string, at time.Time) error {
			return s.Update(key, at, time.Minute, func(v string, live bool) (string, bool, error) { return key, true, nil }, nil)
		}
		if err := set("a", now); err != nil {
			t.Fatal(err)
		}
		err := set("b", now.Add(time.Minute))
		if reject && !errors.Is(err, ErrBindingCapacity) {
			t.Fatal("live binding was evicted")
		}
		if !reject && err != nil {
			t.Fatal(err)
		}
		if err := set("after-expiry", now.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if len(s.entries) != 1 || s.entries["after-expiry"].Value != "after-expiry" {
			t.Fatal("expired entries not reclaimed")
		}
	}
}

func TestSharedBindingStoreFailedCommitRestoresCapacityCleanup(t *testing.T) {
	now := time.Now()
	for _, expired := range []bool{false, true} {
		seen := now
		if expired {
			seen = now.Add(-2 * time.Hour)
		}
		original := Binding[string]{Value: "original", LastSeen: seen, Version: 3}
		entries := map[string]Binding[string]{"old": original}
		s := NewBindingStore[string](BindingPolicy{TTL: time.Hour, MaxEntries: 1}, entries)
		deletes := 0
		s.onDelete = func(string) { deletes++ }
		failed := errors.New("commit failed")
		err := s.Update("new", now, time.Minute, func(string, bool) (string, bool, error) {
			return "new", true, nil
		}, func() error { return failed })
		if !errors.Is(err, failed) || len(entries) != 1 || entries["old"] != original || deletes != 0 || s.version != 3 {
			t.Fatal("failed commit leaked cleanup, deletion or generation", expired, err, entries, deletes)
		}
	}
}
