package groups

import (
	"context"
	"encoding/json"
	"testing"
)

type playgroundNoAuthority struct{}

func (playgroundNoAuthority) Update(context.Context, string, func(json.RawMessage) (json.RawMessage, error)) error {
	panic("snapshot must not update authority")
}
func (playgroundNoAuthority) View(context.Context, string) (json.RawMessage, error) {
	panic("local snapshot must not query authority")
}

func TestPlaygroundSnapshotNeverReloadsAuthority(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Create("vip"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAccountGroups("u1", []string{"vip"}); err != nil {
		t.Fatal(err)
	}
	s.SetSharedAuthority(playgroundNoAuthority{}, false)
	groups, members := s.PlaygroundSnapshot()
	if len(groups) != 2 || len(members["u1"]) != 1 || members["u1"][0] != "vip" {
		t.Fatalf("snapshot=%+v %+v", groups, members)
	}
	members["u1"][0] = "changed"
	_, fresh := s.PlaygroundSnapshot()
	if fresh["u1"][0] != "vip" {
		t.Fatal("snapshot shared mutable memberships")
	}
}
