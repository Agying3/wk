package groups

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestChangeAccountsAtomicMergeAndPersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := s.CreateWithRegion(name, "cn"); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if _, err := s.ChangeAccounts(map[string]string{"u": "cn"}, []string{name}, "append"); err != nil {
				t.Error(err)
			}
		}(name)
	}
	wg.Wait()
	want := []string{"a", "b", "default"}
	if got := s.AccountGroups("u"); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	reloaded, err := New(dir)
	if err != nil || !reflect.DeepEqual(reloaded.AccountGroups("u"), want) {
		t.Fatalf("reload failed: %v", err)
	}
	before := s.AccountGroups("u")
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	s.accFp = filepath.Join(blocked, "account-groups.json")
	if _, err := s.ChangeAccounts(map[string]string{"u": "cn"}, []string{"b"}, "replace"); err == nil {
		t.Fatal("write failure hidden")
	}
	if !reflect.DeepEqual(s.AccountGroups("u"), before) {
		t.Fatal("failed write changed memory")
	}
}

func TestChangeAccountsValidationAndDefaultRegion(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWithRegion("global", "global"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGroupRegion("default", "cn"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountGroups("u", []string{"global"}); err != nil {
		t.Fatal(err)
	}
	issues, err := s.ChangeAccounts(map[string]string{"u": "global"}, []string{"global"}, "remove")
	if err != nil || issues["u"] == nil || !reflect.DeepEqual(s.AccountGroups("u"), []string{"global"}) {
		t.Fatalf("incompatible default accepted: %v %v", issues, err)
	}
	for _, mode := range []string{"bad", "replace"} {
		if _, err := s.ChangeAccounts(map[string]string{"u": "global"}, []string{"missing"}, mode); err == nil {
			t.Fatal("invalid target accepted")
		}
	}
}
