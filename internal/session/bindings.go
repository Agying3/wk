package session

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// Binding is the common lease envelope for every affinity consumer. The JSON
// names retain compatibility with the deployed encrypted prompt binding store.
type Binding[V any] struct {
	Value    V         `json:"choices"`
	LastSeen time.Time `json:"last_seen"`
	Version  uint64    `json:"version,omitempty"`
}

type BindingPolicy struct {
	TTL        time.Duration
	MaxEntries int
	// Accounts may evict cold bindings; immutable prompt selections must reject
	// new allocations instead of silently changing a still-live conversation.
	RejectWhenFull bool
}

func (b Binding[V]) Expired(now time.Time, ttl time.Duration) bool { return now.Sub(b.LastSeen) > ttl }

var ErrBindingCapacity = errors.New("session binding capacity reached")

// BindingStore owns locking, expiry, capacity, generations and commit rollback.
// Account selection and prompt weighting supply values and persistence adapters;
// neither implements its own binding lifecycle. Callbacks run under the lock.
type BindingStore[V any] struct {
	mu       sync.RWMutex
	entries  map[string]Binding[V]
	version  uint64
	policy   BindingPolicy
	onDelete func(string)
}

func NewBindingStore[V any](policy BindingPolicy, entries map[string]Binding[V]) *BindingStore[V] {
	if entries == nil {
		entries = map[string]Binding[V]{}
	}
	s := &BindingStore[V]{entries: entries, policy: policy}
	for _, e := range entries {
		if e.Version > s.version {
			s.version = e.Version
		}
	}
	return s
}

func (s *BindingStore[V]) expired(e Binding[V], now time.Time) bool {
	return e.Expired(now, s.policy.TTL)
}
func (s *BindingStore[V]) deleteLocked(key string) {
	delete(s.entries, key)
	if s.onDelete != nil {
		s.onDelete(key)
	}
}
func (s *BindingStore[V]) pruneLocked(now time.Time) int {
	n := 0
	for key, e := range s.entries {
		if s.expired(e, now) {
			s.deleteLocked(key)
			n++
		}
	}
	return n
}
func (s *BindingStore[V]) roomLocked(key string, now time.Time) error {
	return s.makeRoomLocked(key, now, s.deleteLocked)
}
func (s *BindingStore[V]) makeRoomLocked(key string, now time.Time, remove func(string)) error {
	if _, exists := s.entries[key]; exists {
		return nil
	}
	if s.policy.MaxEntries <= 0 || len(s.entries) < s.policy.MaxEntries {
		return nil
	}
	for k, e := range s.entries {
		if s.expired(e, now) {
			remove(k)
		}
	}
	if len(s.entries) < s.policy.MaxEntries {
		return nil
	}
	if s.policy.RejectWhenFull {
		return ErrBindingCapacity
	}
	type cold struct {
		key string
		at  time.Time
	}
	all := make([]cold, 0, len(s.entries))
	for k, e := range s.entries {
		all = append(all, cold{k, e.LastSeen})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	n := len(s.entries) - s.policy.MaxEntries + s.policy.MaxEntries/8 + 1
	for i := 0; i < n && i < len(all); i++ {
		remove(all[i].key)
	}
	return nil
}
func (s *BindingStore[V]) setLocked(key string, value V, now time.Time) Binding[V] {
	s.version++
	e := Binding[V]{Value: value, LastSeen: now, Version: s.version}
	s.entries[key] = e
	return e
}

func (s *BindingStore[V]) replaceCurrentLocked(key string, version uint64, value V, now time.Time) (Binding[V], bool) {
	e, ok := s.entries[key]
	if !ok || e.Version != version || s.expired(e, now) {
		return Binding[V]{}, false
	}
	return s.setLocked(key, value, now), true
}
func (s *BindingStore[V]) touchCurrentLocked(key string, version uint64, now time.Time) bool {
	e, ok := s.entries[key]
	if !ok || e.Version != version || s.expired(e, now) {
		return false
	}
	e.LastSeen = now
	s.entries[key] = e
	return true
}
func (s *BindingStore[V]) removeCurrentLocked(key string, version uint64) bool {
	e, ok := s.entries[key]
	if !ok || e.Version != version {
		return false
	}
	s.deleteLocked(key)
	return true
}

// Update serializes a domain's read/modify/write transaction. persist must
// durably commit before success when required by that domain. A failed commit
// restores the previous value; no client observes a successful transient bind.
// change must copy reference values before modifying them, so rollback can keep
// the original binding intact. persist must not call back into this store.
func (s *BindingStore[V]) Update(key string, now time.Time, renewInterval time.Duration,
	change func(V, bool) (V, bool, error), persist func() error) error {
	return s.UpdateAlias(key, "", now, renewInterval, change, persist)
}

func (s *BindingStore[V]) UpdateAlias(key, legacy string, now time.Time, renewInterval time.Duration,
	change func(V, bool) (V, bool, error), persist func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.entries[key]
	previous := old
	live := exists && !s.expired(old, now)
	alias, aliasExists := s.entries[legacy]
	migrating := !live && legacy != "" && legacy != key && aliasExists && !s.expired(alias, now)
	var value V
	if live {
		value = old.Value
	}
	if migrating {
		value = alias.Value
		live = true
	}
	value, dirty, err := change(value, live)
	if err != nil {
		return err
	}
	dirty = dirty || migrating
	if !dirty && live && now.Sub(old.LastSeen) < renewInterval {
		return nil
	}
	if !dirty && !live {
		return nil
	}
	if migrating {
		delete(s.entries, legacy)
	}
	removed := map[string]Binding[V]{}
	restoreRemoved := func() {
		for k, e := range removed {
			s.entries[k] = e
		}
	}
	if err := s.makeRoomLocked(key, now, func(k string) {
		removed[k] = s.entries[k]
		delete(s.entries, k)
	}); err != nil {
		restoreRemoved()
		if migrating {
			s.entries[legacy] = alias
		}
		return err
	}
	previousVersion := s.version
	if !dirty && live {
		old.LastSeen = now
		s.entries[key] = old
	} else {
		s.setLocked(key, value, now)
	}
	if persist != nil {
		if err := persist(); err != nil {
			restoreRemoved()
			if exists {
				s.entries[key] = previous
			} else {
				delete(s.entries, key)
			}
			if migrating {
				s.entries[legacy] = alias
			}
			s.version = previousVersion
			return err
		}
	}
	if s.onDelete != nil {
		for k := range removed {
			s.onDelete(k)
		}
		if migrating {
			s.onDelete(legacy)
		}
	}
	return nil
}
