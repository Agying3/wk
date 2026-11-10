package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type credentialAuthorityFixture struct {
	mu   sync.Mutex
	data map[string]json.RawMessage
	fail bool
}

func (s *credentialAuthorityFixture) Update(ctx context.Context, key string, fn func(json.RawMessage) (json.RawMessage, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("offline")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.data == nil {
		s.data = map[string]json.RawMessage{}
	}
	next, err := fn(s.data[key])
	if err == nil {
		s.data[key] = append(json.RawMessage(nil), next...)
	}
	return err
}
func (s *credentialAuthorityFixture) View(_ context.Context, key string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(json.RawMessage(nil), s.data[key]...), nil
}
func sharedAuthFixture(t *testing.T, path string) *Auth {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = path
	return a
}

func TestSharedRefreshReloadsAndPersistsBeforeReturning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workbuddy-u.json")
	if err := os.WriteFile(path, []byte(`{"accessToken":"fixture-access-old","refreshToken":"fixture-refresh-old","uid":"u","enterpriseId":"e"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, b, stale := sharedAuthFixture(t, path), sharedAuthFixture(t, path), sharedAuthFixture(t, path)
	store := &credentialAuthorityFixture{}
	defer SetSharedAuthority(store)()
	calls := 0
	refresh := func(c Credentials) (Credentials, error) {
		calls++
		c.AccessToken = "fixture-access-new"
		c.RefreshToken = "fixture-refresh-new"
		return c, nil
	}
	// Maintenance callers already hold this lock; shared refresh must not reenter.
	a.Lock()
	enabled, err := a.RefreshShared(context.Background(), refresh)
	a.Unlock()
	if !enabled || err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err = b.RefreshShared(context.Background(), refresh); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || b.Snapshot().AccessToken != "fixture-access-new" {
		t.Fatal("second process refreshed stale credentials")
	}
	if disk := sharedAuthFixture(t, path); disk.Snapshot().AccessToken != "fixture-access-new" {
		t.Fatal("refresh returned before durable save")
	}
	if err := stale.SaveAtomic(); !errors.Is(err, ErrStaleCredentials) {
		t.Fatalf("stale save=%v", err)
	}
	for _, raw := range store.data {
		if strings.Contains(string(raw), "fixture-") {
			t.Fatal("credential persisted in SQL state")
		}
	}
	store.fail = true
	if _, err := a.RefreshShared(context.Background(), refresh); err == nil {
		t.Fatal("authority failure accepted")
	}
	if calls != 1 {
		t.Fatal("refresh contacted provider without authority")
	}
}

func TestSharedCredentialsCannotResurrectDeletedAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workbuddy-u.json")
	if err := os.WriteFile(path, []byte(`{"accessToken":"fixture-token","uid":"u"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := sharedAuthFixture(t, path)
	defer SetSharedAuthority(&credentialAuthorityFixture{})()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveAtomic(); err == nil {
		t.Fatal("deleted account resurrected")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("deleted account file was created")
	}
}
