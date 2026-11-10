package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"workbuddy2api/internal/redisstore"
)

// SharedAuthority serializes bindings across overlapping versions. The row
// contains only HMAC key digests and account IDs, never conversation text.
type SharedAuthority interface {
	Update(context.Context, string, func(json.RawMessage) (json.RawMessage, error)) error
	View(context.Context, string) (json.RawMessage, error)
}

type sharedBindings struct {
	Format  int              `json:"format"`
	Version uint64           `json:"version"`
	Entries map[string]entry `json:"entries"`
}

// shared operates on a transaction-local router, so legacy binding policy and
// CAS behavior remain identical without publishing a stale local snapshot.
func (r *Router) shared(fn func(*Router) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return r.cfg.Authority.Update(ctx, "session/bindings", func(raw json.RawMessage) (json.RawMessage, error) {
		state := sharedBindings{Format: 1, Entries: map[string]entry{}}
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, err
			}
			if state.Format != 1 || state.Entries == nil {
				return nil, fmt.Errorf("unsupported shared session state")
			}
		}
		if len(state.Entries) > r.cfg.MaxEntries {
			return nil, fmt.Errorf("shared binding limit exceeded")
		}
		cfg := r.cfg
		cfg.Authority = nil
		cfg.Store = redisstore.Noop{}
		b := NewBindingStore[string](BindingPolicy{TTL: cfg.TTL, MaxEntries: cfg.MaxEntries}, state.Entries)
		if state.Version > b.version {
			b.version = state.Version
		}
		local := &Router{BindingStore: b, cfg: cfg}
		if err := fn(local); err != nil {
			return nil, err
		}
		state.Entries, state.Version = b.entries, b.version
		return json.Marshal(state)
	})
}

// ResolveLeaseWithCandidatesChecked returns coordination failures to the
// request handler; callers must not fall back to an uncoordinated allocation.
func (r *Router) ResolveLeaseWithCandidatesChecked(raw string, allow map[string]bool, preferred string, healthy, available []string) (out Lease, ok bool, err error) {
	if r.cfg.Authority == nil {
		out, ok = r.ResolveLeaseWithCandidates(raw, allow, preferred, healthy, available)
		return
	}
	err = r.shared(func(local *Router) error {
		out, ok = local.ResolveLeaseWithCandidates(raw, allow, preferred, healthy, available)
		return nil
	})
	if err != nil {
		return Lease{}, false, err
	}
	return
}

func (r *Router) ReplaceIfCurrentChecked(raw string, old Lease, uid string) (out Lease, ok bool, err error) {
	if r.cfg.Authority == nil {
		out, ok = r.ReplaceIfCurrent(raw, old, uid)
		return
	}
	err = r.shared(func(local *Router) error { out, ok = local.ReplaceIfCurrent(raw, old, uid); return nil })
	if err != nil {
		return Lease{}, false, err
	}
	return
}

// InitializeShared seeds legacy handoff data once. A candidate cannot overwrite
// bindings already created by another version.
func (r *Router) InitializeShared(raw []byte) error {
	if r.cfg.Authority == nil {
		return r.Import(raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return r.cfg.Authority.Update(ctx, "session/bindings", func(current json.RawMessage) (json.RawMessage, error) {
		if len(current) > 0 && string(current) != "null" {
			return current, nil
		}
		cfg := r.cfg
		cfg.Authority = nil
		cfg.Store = redisstore.Noop{}
		local := New(cfg)
		if len(raw) > 0 {
			if err := local.Import(raw); err != nil {
				return nil, err
			}
		}
		return json.Marshal(sharedBindings{Format: 1, Version: local.version, Entries: local.entries})
	})
}
