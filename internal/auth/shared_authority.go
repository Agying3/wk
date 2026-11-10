package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SharedAuthority serializes account updates across overlap workers. Records
// contain credential digests and generations; tokens remain in the auth file.
type SharedAuthority interface {
	Update(context.Context, string, func(json.RawMessage) (json.RawMessage, error)) error
	View(context.Context, string) (json.RawMessage, error)
}

var sharedAuthorityConfig struct {
	sync.RWMutex
	store SharedAuthority
}

var ErrStaleCredentials = errors.New("credential file changed; stale write refused")
var ErrSharedAccountChanged = errors.New("shared credential account changed")

// SetSharedAuthority opts this process into coordinated persistence. Call at
// startup before serving; the returned closure restores the previous setting.
func SetSharedAuthority(store SharedAuthority) func() {
	sharedAuthorityConfig.Lock()
	previous := sharedAuthorityConfig.store
	sharedAuthorityConfig.store = store
	sharedAuthorityConfig.Unlock()
	return func() {
		sharedAuthorityConfig.Lock()
		sharedAuthorityConfig.store = previous
		sharedAuthorityConfig.Unlock()
	}
}

func currentSharedAuthority() SharedAuthority {
	sharedAuthorityConfig.RLock()
	defer sharedAuthorityConfig.RUnlock()
	return sharedAuthorityConfig.store
}

type sharedCredentialRecord struct {
	Version    int    `json:"version"`
	Generation uint64 `json:"generation"`
	Digest     string `json:"digest"`
}

func credentialDigest(c Credentials) string {
	raw, _ := json.Marshal([]any{c.UID, c.EnterpriseID, c.Domain, c.AccessToken, c.RefreshToken, c.ExpiresAt, c.Authority})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func credentialKey(path string) (string, error) {
	if path == "" {
		return "", errors.New("shared credentials require a durable file")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(absolute)))
	return "auth/" + hex.EncodeToString(sum[:]), nil
}

func credentialRecord(raw json.RawMessage) (sharedCredentialRecord, error) {
	var record sharedCredentialRecord
	if len(raw) != 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &record); err != nil {
			return record, errors.New("invalid shared credential record")
		}
		if record.Version != 1 {
			return record, errors.New("unsupported shared credential version")
		}
	}
	record.Version = 1
	return record, nil
}

func readSharedCredential(path string) (Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	a, err := Parse(raw)
	if err != nil {
		return Credentials{}, err
	}
	a.FilePath = path
	return a.Snapshot(), nil
}

func (a *Auth) installShared(c Credentials) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.AccessToken, a.RefreshToken, a.ExpiresAt = c.AccessToken, c.RefreshToken, c.ExpiresAt
	a.Domain, a.UID, a.EnterpriseID = c.Domain, c.UID, c.EnterpriseID
	a.Nickname, a.DeviceID, a.DeviceProfile = c.Nickname, c.DeviceID, c.DeviceProfile
	a.Authority, a.ImportDisabled = c.Authority, c.ImportDisabled
	a.sharedBaseline = credentialDigest(c)
}

// RefreshShared covers reload, refresh and durable commit in one authority
// transaction. It never acquires Auth.Lock: maintenance callers may hold it.
func (a *Auth) RefreshShared(ctx context.Context, refresh func(Credentials) (Credentials, error)) (bool, error) {
	store := currentSharedAuthority()
	if store == nil {
		return false, nil
	}
	before := a.Snapshot()
	key, err := credentialKey(before.FilePath)
	if err != nil {
		return true, err
	}
	err = store.Update(ctx, key, func(raw json.RawMessage) (json.RawMessage, error) {
		record, err := credentialRecord(raw)
		if err != nil {
			return nil, err
		}
		current, err := readSharedCredential(before.FilePath)
		if err != nil {
			return nil, err
		}
		if current.UID != before.UID || current.EnterpriseID != before.EnterpriseID {
			return nil, ErrSharedAccountChanged
		}
		next := current
		if credentialDigest(current) == credentialDigest(before) {
			next, err = refresh(current)
			if err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if next.UID != current.UID || next.EnterpriseID != current.EnterpriseID {
				return nil, ErrSharedAccountChanged
			}
			latest, err := readSharedCredential(before.FilePath)
			if err != nil {
				return nil, err
			}
			if credentialDigest(latest) != credentialDigest(current) {
				return nil, ErrStaleCredentials
			}
			next.FilePath = before.FilePath
			next.Nickname, next.ImportDisabled = current.Nickname, current.ImportDisabled
			err = a.saveAtomicSnapshot(next)
			if err != nil {
				return nil, err
			}
		}
		record.Generation++
		record.Digest = credentialDigest(next)
		a.installShared(next)
		return json.Marshal(record)
	})
	return true, err
}

func (a *Auth) saveShared(store SharedAuthority, snapshot Credentials) error {
	key, err := credentialKey(snapshot.FilePath)
	if err != nil {
		return err
	}
	a.mu.RLock()
	baseline := a.sharedBaseline
	a.mu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return store.Update(ctx, key, func(raw json.RawMessage) (json.RawMessage, error) {
		record, err := credentialRecord(raw)
		if err != nil {
			return nil, err
		}
		current, err := readSharedCredential(snapshot.FilePath)
		if err != nil {
			if !os.IsNotExist(err) || baseline != "" || record.Generation != 0 {
				return nil, err
			}
		} else {
			if current.UID != snapshot.UID || current.EnterpriseID != snapshot.EnterpriseID {
				return nil, ErrSharedAccountChanged
			}
			actual := credentialDigest(current)
			if actual != baseline && actual != credentialDigest(snapshot) {
				return nil, ErrStaleCredentials
			}
			snapshot.Nickname, snapshot.ImportDisabled = current.Nickname, current.ImportDisabled
		}
		if err := a.saveAtomicSnapshot(snapshot); err != nil {
			return nil, err
		}
		record.Generation++
		record.Digest = credentialDigest(snapshot)
		a.mu.Lock()
		a.sharedBaseline = record.Digest
		a.mu.Unlock()
		return json.Marshal(record)
	})
}

// WithCredentialWrite serializes management import/delete/metadata renames
// against refresh. The callback may modify the file, but must not recursively
// call SaveAtomic or RefreshShared. Existing serial deployments call fn directly.
func WithCredentialWrite(ctx context.Context, path string, fn func() error) error {
	store := currentSharedAuthority()
	if store == nil {
		return fn()
	}
	key, err := credentialKey(path)
	if err != nil {
		return err
	}
	return store.Update(ctx, key, func(raw json.RawMessage) (json.RawMessage, error) {
		record, err := credentialRecord(raw)
		if err != nil {
			return nil, err
		}
		if err := fn(); err != nil {
			return nil, err
		}
		record.Generation++
		record.Digest = ""
		if current, err := readSharedCredential(path); err == nil {
			record.Digest = credentialDigest(current)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		return json.Marshal(record)
	})
}
