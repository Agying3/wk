package groups

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type groupAuthorityFixture struct {
	mu   sync.Mutex
	data map[string]json.RawMessage
	fail bool
}

func (s *groupAuthorityFixture) Update(_ context.Context, key string, fn func(json.RawMessage) (json.RawMessage, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("offline")
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
func (s *groupAuthorityFixture) View(_ context.Context, key string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(json.RawMessage(nil), s.data[key]...), nil
}

func TestSharedGroupsCandidateDoesNotWriteAndReadsLatest(t *testing.T) {
	dir := t.TempDir()
	owner, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NewReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	authority := &groupAuthorityFixture{}
	owner.SetSharedAuthority(authority, true)
	candidate.SetSharedAuthority(authority, false)
	if err := candidate.Create("blocked"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("candidate write=%v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "groups.json"))
	if string(before) != string(after) {
		t.Fatal("candidate changed file")
	}
	if err := owner.Create("fresh"); err != nil {
		t.Fatal(err)
	}
	if err := owner.SetAccountGroups("u", []string{"fresh"}); err != nil {
		t.Fatal(err)
	}
	if err := owner.SetProxyBindings("fresh", []string{"proxy-one"}); err != nil {
		t.Fatal(err)
	}
	key, err := owner.CreateKey("test", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := candidate.LookupKey(key.Key); err != nil || got.Group != "fresh" {
		t.Fatalf("candidate stale key: %v", err)
	}
	if !candidate.AccountsInGroup("fresh")["u"] {
		t.Fatal("candidate stale membership")
	}
	if got := candidate.ProxyBindingsOf("fresh"); len(got) != 1 || got[0] != "proxy-one" {
		t.Fatal("candidate stale egress binding")
	}
	if _, err := owner.ChangeAccounts(map[string]string{"u": RegionCN}, []string{DefaultGroup}, "replace"); err != nil {
		t.Fatal(err)
	}
	if candidate.AccountsInGroup("fresh")["u"] {
		t.Fatal("candidate stale batch membership")
	}
	if err := owner.DeleteKey(key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := candidate.LookupKey(key.Key); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked key remains authorized")
	}
	authority.fail = true
	if _, err := candidate.LookupKey(key.Key); err == nil {
		t.Fatal("authorization did not fail closed")
	}
	if got := candidate.RegisteredGroups("u"); len(got) == 0 {
		t.Fatal("outage became unrestricted egress")
	}
	if got := candidate.ProxyBindingsOf("fresh"); len(got) == 0 {
		t.Fatal("outage became unrestricted proxy selection")
	}
	if got := candidate.AccountsInGroup("fresh"); got == nil || len(got) != 0 {
		t.Fatal("outage became unrestricted account selection")
	}
}
