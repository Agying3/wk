package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func deviceFixture(t *testing.T) (*Auth, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workbuddy-fixture.json")
	raw := []byte(`{"uid":"fixture-uid","accessToken":"fake-at","refreshToken":"fake-rt","provider_extra":{"keep":true}}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = path
	return a, path
}

func TestDeviceIdentityPersistsAndPreservesProviderFields(t *testing.T) {
	a, path := deviceFixture(t)
	id := a.DeviceIdentity()
	if !validDeviceIdentity(id) {
		t.Fatalf("invalid persisted ID: %q", id)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	if doc["provider_extra"] == nil || doc["accessToken"] != "fake-at" {
		t.Fatal("initializing identity changed credential/provider fields")
	}
	reloaded, err := Parse(raw)
	if err != nil || reloaded.DeviceIdentity() != id {
		t.Fatalf("identity did not survive parse: %v", err)
	}
	reloaded.FilePath = path
	reloaded.ApplyRefresh("fake-new-at", "fake-new-rt", "", 9999999999)
	if err := reloaded.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadDir(filepath.Dir(path), RegionAll)
	if err != nil || len(loaded) != 1 || loaded[0].DeviceIdentity() != id {
		t.Fatalf("refresh/restart changed identity: %v", err)
	}
}

func TestDeviceIdentityConcurrentReloadsAndStaleSave(t *testing.T) {
	a, path := deviceFixture(t)
	raw, _ := os.ReadFile(path)
	stale, _ := Parse(raw)
	stale.FilePath = path
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for i := 0; i < cap(ids); i++ {
		copy, _ := Parse(raw)
		copy.FilePath = path
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- copy.DeviceIdentity()
		}()
	}
	wg.Wait()
	close(ids)
	expected := a.DeviceIdentity()
	for id := range ids {
		if id != expected || id == "" {
			t.Fatal("concurrent reloads created different devices")
		}
	}
	if err := stale.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	if stale.Snapshot().DeviceID != expected || savedDeviceIdentity(path, a.UID) != expected {
		t.Fatal("stale credential save erased the persisted identity")
	}
}

func TestDeviceIdentityPersistenceFailureKeepsLegacyFallback(t *testing.T) {
	a := &Auth{UID: "fixture-uid", FilePath: filepath.Join(t.TempDir(), "missing.json")}
	if a.DeviceIdentity() != "" || a.DeviceIdentity() != "" {
		t.Fatal("failed persistence exposed a temporary random identity")
	}
}
