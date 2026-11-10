//go:build linux

package lifecycle

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOverlapMembershipHelper(t *testing.T) {
	if os.Getenv("WB_MEMBERSHIP_HELPER") != "1" {
		return
	}
	m, err := JoinOverlap(os.Getenv("WB_MEMBER_LOCK"), os.Getenv("WB_MEMBER_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.Domain != os.Getenv("WB_MEMBER_DOMAIN") {
		t.Fatal("domain changed across processes")
	}
	ok, err := m.ClaimOwner()
	if err != nil || ok {
		t.Fatal("candidate stole live management ownership")
	}
	ok, err = m.Alive(os.Getenv("WB_MEMBER_PARENT"))
	if err != nil || !ok {
		t.Fatal("live parent misclassified dead")
	}
}

func TestOverlapMembershipExcludesLegacyAndPreservesOwnership(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "writer.lock")
	coord := filepath.Join(dir, "coord")
	unlock, err := Lock(lock)
	if err != nil {
		t.Fatal(err)
	}
	if m, e := JoinOverlap(lock, coord); e == nil {
		m.Close()
		t.Fatal("legacy writer was bypassed")
	}
	unlock()
	m, err := JoinOverlap(lock, coord)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, e := Lock(lock); e == nil {
		t.Fatal("legacy writer entered overlap domain")
	}
	owner, err := m.ClaimOwner()
	if err != nil || !owner {
		t.Fatal(owner, err)
	}
	if other, e := JoinOverlap(lock, filepath.Join(dir, "split")); e == nil {
		other.Close()
		t.Fatal("split coordination directory accepted")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestOverlapMembershipHelper$")
	cmd.Env = append(os.Environ(), "WB_MEMBERSHIP_HELPER=1", "WB_MEMBER_LOCK="+lock, "WB_MEMBER_DIR="+coord, "WB_MEMBER_DOMAIN="+m.Domain, "WB_MEMBER_PARENT="+m.ID)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("second process: %v %s", e, out)
	}
	id := m.ID
	m.Close()
	next, err := JoinOverlap(lock, coord)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if live, e := next.Alive(id); e != nil || live {
		t.Fatal("dead owner was not reclaimable", e)
	}
	if owner, e := next.ClaimOwner(); e != nil || !owner {
		t.Fatal("next owner failed", e)
	}
}

func TestOverlapDomainSurvivesRuntimeLockDirectoryRecreation(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "writer.lock")
	coord := filepath.Join(dir, "members")
	a, err := JoinOverlap(lock, coord)
	if err != nil {
		t.Fatal(err)
	}
	domain := a.Domain
	a.Close()
	for _, path := range []string{lock, lock + ".overlap-init", lock + ".overlap-domain"} {
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	b, err := JoinOverlap(lock, coord)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Domain != domain {
		t.Fatal("reboot would select an empty PostgreSQL namespace and replay stale legacy state")
	}
	if other, e := JoinOverlap(filepath.Join(dir, "other-writer.lock"), coord); e == nil {
		other.Close()
		t.Fatal("persistent domain accepted a different legacy writer lock")
	}
}
