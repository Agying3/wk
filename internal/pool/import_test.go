package pool

import (
	"errors"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestAddDisabledCheckedAuthorityFence(t *testing.T) {
	p := New("")
	s := &sharedPool{}
	s.fail(errors.New("offline injected authority failure"))
	p.shared.Store(s)
	p.mu.pool = p
	added, err := p.AddDisabledChecked(&auth.Auth{UID: "offline", AccessToken: "fake"}, "import")
	if added || err == nil || p.AuthByUID("offline") != nil {
		t.Fatal("a fenced authority must reject the insertion")
	}
	if p.AddDisabled(&auth.Auth{UID: "offline"}, "import") {
		t.Fatal("compatibility API reported success through the authority fence")
	}
}

func TestAddDisabledCheckedDoesNotOverwrite(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "offline", AccessToken: "original"}
	if added, err := p.AddDisabledChecked(a, "import"); !added || err != nil {
		t.Fatal("new disabled account was not installed")
	}
	if added, err := p.AddDisabledChecked(&auth.Auth{UID: "offline", AccessToken: "replacement"}, "import"); added || err != nil {
		t.Fatal("duplicate must be skipped")
	}
	if p.AuthByUID(a.UID) != a || !p.IsDisabled(a.UID) || p.Pick() != nil {
		t.Fatal("duplicate changed the credential or disabled state")
	}
}
