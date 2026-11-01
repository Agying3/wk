package pool

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestFlushWithErrorRetainsFileChangesForRetry(t *testing.T) {
	p := New("") // no background flusher in this test
	p.stateFp = filepath.Join(t.TempDir(), "state.json")
	p.Add(&auth.Auth{UID: "synthetic", ExpiresAt: 9999999999})
	p.Disable("synthetic", "manual disabled")
	if err := os.Mkdir(p.stateFp, 0700); err != nil {
		t.Fatal(err)
	}
	if err := p.FlushWithError(); err == nil {
		t.Fatal("rename failure hidden")
	}
	if !p.dirty.Load() {
		t.Fatal("failed file snapshot no longer scheduled for retry")
	}
	if err := os.Remove(p.stateFp); err != nil {
		t.Fatal(err)
	}
	if err := p.FlushWithError(); err != nil {
		t.Fatal(err)
	}
	if p.dirty.Load() {
		t.Fatal("successful flush still dirty")
	}
	raw, err := os.ReadFile(p.stateFp)
	if err != nil || len(raw) == 0 {
		t.Fatalf("no persisted snapshot: %v", err)
	}
}
