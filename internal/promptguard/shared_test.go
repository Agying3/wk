package promptguard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/coordination"
)

func guardCoordinator(t *testing.T) *coordination.Store {
	t.Helper()
	dsn := os.Getenv("WB2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set WB2API_TEST_POSTGRES_DSN to an isolated PostgreSQL database")
	}
	s, err := coordination.OpenPostgres(context.Background(), dsn, fmt.Sprintf("promptguard-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSharedCapturesRetainBothReleasesAndEncryptedPayload(t *testing.T) {
	s := guardCoordinator(t)
	path := filepath.Join(t.TempDir(), "guard.enc")
	a, err := OpenShared(path, "isolated-test-secret", s, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenShared(path, "isolated-test-secret", s, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := a
			if i%2 != 0 {
				m = b
			}
			if _, e := m.Capture("test-model", "u", messages("private capture text"), messages("private capture text")); e != nil {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	items := a.List()
	if len(items) != 1 || items[0].Occurrences != 20 {
		t.Fatalf("captures overwritten: %#v", items)
	}
	if _, err = b.Capture("another-model", "u", messages("other"), messages("other")); err != nil {
		t.Fatal(err)
	}
	if err = a.FlushChecked(); err != nil {
		t.Fatal(err)
	}
	if len(b.List()) != 2 {
		t.Fatal("old owner flush lost relay capture")
	}
	config := a.Config()
	config.Rules = []Rule{variantRule()}
	if err = a.Update(config); err != nil {
		t.Fatal(err)
	}
	first := variantDocument()
	if err = a.ApplySession(first, "shared-conversation"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		doc := variantDocument()
		if err = b.ApplySession(doc, "shared-conversation"); err != nil {
			t.Fatal(err)
		}
		if Text(Extract(doc)) != Text(Extract(first)) {
			t.Fatal("relay changed the established prompt-session choice")
		}
	}
	raw, err := s.View(context.Background(), "promptguard/state")
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"private capture text", "test-model", "other"} {
		if contains := strings.Contains(string(raw), plain); contains {
			t.Fatal("shared prompt payload is not encrypted")
		}
	}
	// Failure must reject writes, never flush a stale process-local cache.
	s.Close()
	if _, err = b.Capture("failure", "u", messages("x"), messages("x")); err == nil {
		t.Fatal("capture accepted unavailable authority")
	}
	if err = b.ApplySession(map[string]any{"messages": []any{}}, "session"); err == nil {
		t.Fatal("rewrite used stale shared state")
	}
}

func TestSharedPromptRelayRequiresInitializedOwner(t *testing.T) {
	s := guardCoordinator(t)
	if m, err := OpenShared(filepath.Join(t.TempDir(), "guard.enc"), "isolated-test-secret", s, false); err == nil {
		m.Close()
		t.Fatal("relay bootstrapped shared state")
	}
}
