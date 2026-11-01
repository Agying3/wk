package server

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

func TestAccountDeviceFirstLoginAndRelogin(t *testing.T) {
	h := newTestHandler(t, Config{AuthDir: t.TempDir(), Pool: pool.New(""), Region: "cn"})
	cred := accountCredential{UID: "new-user", AccessToken: "fake-at", RefreshToken: "fake-rt", Domain: "www.codebuddy.cn"}
	if _, _, err := h.persistAccount(cred, "cn"); err != nil {
		t.Fatal(err)
	}
	a := h.cfg.Pool.AuthByUID(cred.UID)
	before, id := a.Device(), a.Snapshot().DeviceID
	if !before.IsMac() || id == "" {
		t.Fatal("new account did not receive a saved Mac device")
	}
	cred.AccessToken, cred.RefreshToken = "rotated-at", "rotated-rt"
	if _, _, err := h.persistAccount(cred, "cn"); err != nil {
		t.Fatal(err)
	}
	a = h.cfg.Pool.AuthByUID(cred.UID)
	if a.Device() != before || a.Snapshot().DeviceID != id || a.Snapshot().AccessToken != "rotated-at" {
		t.Fatal("re-login changed device or failed to replace credentials")
	}
	loaded, err := auth.LoadDir(h.cfg.AuthDir, auth.RegionAll)
	if err != nil || len(loaded) != 1 || loaded[0].Device() != before {
		t.Fatal("restart changed allocated device")
	}
}

func TestAccountDeviceLegacyReloginPreservesWindows(t *testing.T) {
	for _, usePool := range []bool{true, false} {
		dir := t.TempDir()
		path := filepath.Join(dir, "workbuddy-existing.json")
		raw := []byte(`{"uid":"existing","accessToken":"old-fake-at","refreshToken":"old-fake-rt","_device_identity":"0123456789abcdef0123456789abcdef0123"}`)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		a, err := auth.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		a.FilePath = path
		before := a.Device()
		h := newTestHandler(t, Config{AuthDir: dir, Pool: pool.New(""), Region: "cn"})
		if usePool {
			h.cfg.Pool.Add(a)
		}
		if _, _, err := h.persistAccount(accountCredential{UID: "existing", AccessToken: "new-fake-at", Domain: "www.codebuddy.cn"}, "cn"); err != nil {
			t.Fatal(err)
		}
		current := h.cfg.Pool.AuthByUID("existing")
		if current.Device() != before || current.Device().IsMac() {
			t.Fatal("legacy re-login received new Mac defaults")
		}
	}
}
