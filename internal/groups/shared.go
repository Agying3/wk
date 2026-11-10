package groups

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"
)

type SharedAuthority interface {
	Update(context.Context, string, func(json.RawMessage) (json.RawMessage, error)) error
	View(context.Context, string) (json.RawMessage, error)
}

var ErrReadOnly = errors.New("management state belongs to the active owner")

// Deliberately invalid as either a region or proxy group: legacy read APIs
// cannot return errors and must never turn an outage into unrestricted egress.
const sharedUnavailable = "__shared_authority_unavailable__"

// CheckShared lets request entry points report a storage outage explicitly.
func (s *Store) CheckShared() error { return s.reloadShared() }

// SetSharedAuthority synchronizes file snapshots with the sole management
// owner's writes. Every overlap process must use the same shared data directory.
func (s *Store) SetSharedAuthority(store SharedAuthority, writable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shared, s.readOnly = store, !writable
	path, _ := filepath.Abs(filepath.Dir(s.fp))
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	s.sharedKey = "management/groups/" + hex.EncodeToString(sum[:])
}

func (s *Store) SetWritable(writable bool) { s.mu.Lock(); s.readOnly = !writable; s.mu.Unlock() }

func (s *Store) reloadShared() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shared == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.shared.Update(ctx, s.sharedKey, func(raw json.RawMessage) (json.RawMessage, error) {
		fresh := &Store{fp: s.fp, accFp: s.accFp, keysFp: s.keysFp, regionFp: s.regionFp, pgFp: s.pgFp,
			accountOf: map[string][]string{}, regions: map[string]string{}, pgBindings: map[string][]string{}}
		if err := fresh.loadAll(); err != nil {
			return nil, err
		}
		if !fresh.hasGroupLocked(DefaultGroup) {
			fresh.groups = append(fresh.groups, DefaultGroup)
		}
		s.groups, s.accountOf, s.keys = fresh.groups, fresh.accountOf, fresh.keys
		s.regions, s.pgBindings = fresh.regions, fresh.pgBindings
		if len(raw) == 0 || string(raw) == "null" {
			return json.RawMessage(`{"version":1}`), nil
		}
		return raw, nil
	})
}

func (s *Store) prepareWrite() error {
	if err := s.reloadShared(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.readOnly {
		return ErrReadOnly
	}
	return nil
}

func (s *Store) persistSharedLocked() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.shared.Update(ctx, s.sharedKey, func(raw json.RawMessage) (json.RawMessage, error) {
		if err := s.persistFilesLocked(); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"version":1}`), nil
	})
}

func (s *Store) persistAccountBatchLocked(raw []byte) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if s.shared == nil {
		return writeFileAtomic(s.accFp, raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.shared.Update(ctx, s.sharedKey, func(current json.RawMessage) (json.RawMessage, error) {
		if err := writeFileAtomic(s.accFp, raw); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"version":1}`), nil
	})
}
