package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

type bindingAuthorityFixture struct {
	mu   sync.Mutex
	data map[string]json.RawMessage
	fail bool
}

func (s *bindingAuthorityFixture) Update(ctx context.Context, key string, fn func(json.RawMessage) (json.RawMessage, error)) error {
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
	next, err := fn(append(json.RawMessage(nil), s.data[key]...))
	if err == nil {
		s.data[key] = append(json.RawMessage(nil), next...)
	}
	return err
}
func (s *bindingAuthorityFixture) View(_ context.Context, key string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(json.RawMessage(nil), s.data[key]...), nil
}

func TestSharedBindingsRejectOldVersionCompletion(t *testing.T) {
	authority := &bindingAuthorityFixture{}
	old := New(Config{Authority: authority, Salt: []byte("shared")})
	next := New(Config{Authority: authority, Salt: []byte("shared")})
	lease, ok, err := old.ResolveLeaseWithCandidatesChecked("private-conversation", nil, "A", []string{"A", "B"}, []string{"A", "B"})
	if err != nil || !ok {
		t.Fatalf("resolve: %v", err)
	}
	newLease, ok, err := next.ReplaceIfCurrentChecked("private-conversation", lease, "B")
	if err != nil || !ok || newLease.Version <= lease.Version {
		t.Fatalf("replace: %v", err)
	}
	if old.TouchIfCurrent("private-conversation", lease) || old.UnbindIfCurrent("private-conversation", lease) {
		t.Fatal("old process changed new generation")
	}
	if _, ok, err := old.ReplaceIfCurrentChecked("private-conversation", lease, "A"); err != nil || ok {
		t.Fatal("stale replacement succeeded")
	}
	got, ok := old.LookupLease("private-conversation")
	if !ok || got.UID != "B" {
		t.Fatal("did not read shared owner")
	}
	next.Unbind("private-conversation")
	back, _, err := old.ResolveLeaseWithCandidatesChecked("private-conversation", nil, "A", []string{"A"}, []string{"A"})
	if err != nil || back.Version <= newLease.Version {
		t.Fatal("generation reset after deletion")
	}
	for _, raw := range authority.data {
		if strings.Contains(string(raw), "private-conversation") {
			t.Fatal("raw conversation persisted")
		}
	}
}

func TestSharedBindingsImportOnceAndFailClosed(t *testing.T) {
	authority := &bindingAuthorityFixture{}
	legacy := New(Config{Salt: []byte("same")})
	legacy.Bind("c", "A")
	raw, _ := legacy.Export()
	r := New(Config{Authority: authority, Salt: []byte("same")})
	if err := r.InitializeShared(raw); err != nil {
		t.Fatal(err)
	}
	r.Bind("c", "B")
	if err := r.Import(raw); err != nil {
		t.Fatal(err)
	}
	if lease, ok := r.LookupLease("c"); !ok || lease.UID != "B" {
		t.Fatal("candidate import overwrote live bindings")
	}
	authority.fail = true
	if _, ok, err := r.ResolveLeaseWithCandidatesChecked("c", nil, "A", []string{"A"}, []string{"A"}); err == nil || ok {
		t.Fatal("coordination failure did not fail closed")
	}
	if _, ok, err := r.ReplaceIfCurrentChecked("c", Lease{UID: "B", Version: 2}, "A"); err == nil || ok {
		t.Fatal("replacement ignored authority failure")
	}
}
