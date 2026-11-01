package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"workbuddy2api/internal/device"
)

func TestDeviceProfileMacSurvivesRefreshReloadAndStaleSave(t *testing.T) {
	id, profile, err := device.NewMac()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "workbuddy-mac.json")
	a := &Auth{UID: "mac-user", AccessToken: "fake-at", RefreshToken: "fake-rt", FilePath: path, DeviceID: id, DeviceProfile: profile}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	reloaded, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.FilePath = path
	reloaded.ApplyRefresh("rotated-at", "rotated-rt", "", 9999999999)
	if err := reloaded.SaveAtomic(); err != nil || reloaded.Device() != profile {
		t.Fatalf("refresh changed device: %v", err)
	}
	// An object loaded before device metadata appeared must not erase it.
	stale := &Auth{UID: a.UID, AccessToken: "another-fake-at", FilePath: path}
	if err := stale.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	if stale.Device() != profile || stale.Snapshot().DeviceID != id {
		t.Fatal("stale save replaced the allocated Mac device")
	}
	raw, _ = os.ReadFile(path)
	var exported map[string]any
	json.Unmarshal(raw, &exported)
	if exported["_device_profile"] == nil {
		t.Fatal("device metadata missing from saved credential")
	}
}

func TestDeviceProfileLegacyWindowsNeverBecomesMac(t *testing.T) {
	a, path := deviceFixture(t)
	before := a.Device()
	if before.IsMac() {
		t.Fatal("old credential received new-account defaults")
	}
	a.ApplyRefresh("rotated", "rotated-r", "", 9999999999)
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadDir(filepath.Dir(path), RegionAll)
	if err != nil || len(loaded) != 1 || loaded[0].Device() != before {
		t.Fatalf("Windows device changed: %v", err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a.Device() != before {
				t.Error("concurrent reads changed legacy device")
			}
		}()
	}
	wg.Wait()
}

func TestDeviceProfileParseRejectsCorruptionAndKeepsBareLegacy(t *testing.T) {
	legacy, err := Parse([]byte(`{"uid":"old","accessToken":"fake-at"}`))
	if err != nil || legacy.Device().IsMac() {
		t.Fatal("bare legacy credential changed platform")
	}
	if _, err := Parse([]byte(`{"uid":"old","accessToken":"fake-at","_device_profile":{"version":1,"platform":"macos"}}`)); err == nil {
		t.Fatal("corrupt profile silently accepted")
	}
}
