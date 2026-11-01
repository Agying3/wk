package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/device"
)

// 重新登录必须沿用磁盘上的设备标识与档案：这是 login.sh 旧实现丢掉的东西。
func TestSaveLoginPreservesExistingDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workbuddy-u1.json")
	t.Setenv("WB2A_LOGIN_AUTH_FILE", path)
	existing := &auth.Auth{
		AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: 1, Domain: "www.codebuddy.cn",
		UID: "u1", FilePath: path,
		DeviceID:      "0123456789abcdef0123456789abcdef0123",
		DeviceProfile: device.LegacyWindows("u1", "0123456789abcdef0123456789abcdef0123"),
	}
	if err := existing.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	result := `{"access_token":"new-at","refresh_token":"new-rt","expires_in":3600,"domain":"www.codebuddy.cn","uid":"u1","account_type":"personal","oneid_account_id":"oid","idp":"idp1","token_type":"Bearer"}`
	if err := saveLogin(strings.NewReader(result)); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.AccessToken != "new-at" || parsed.RefreshToken != "new-rt" || parsed.ExpiresAt <= 1 {
		t.Fatalf("credentials not updated: %+v", parsed.Snapshot())
	}
	// 旧 Windows 档案必须逐字段保留，不能被"新账号默认 Mac"覆盖。
	if parsed.DeviceProfile.Platform != "windows" || parsed.DeviceID != existing.DeviceID {
		t.Fatalf("existing device lost: %q %q", parsed.DeviceProfile.Platform, parsed.DeviceID)
	}
	if parsed.Authority.AccountType != "personal" || parsed.Authority.OneIDAccountID != "oid" || parsed.Authority.IDP != "idp1" || parsed.Authority.TokenType != "Bearer" {
		t.Fatalf("authority metadata lost: %+v", parsed.Authority)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode=%v err=%v", info.Mode().Perm(), err)
	}
}

// 新账号按服务端口径建立 Mac 档案，并把设备元数据写进文件。
func TestSaveLoginCreatesMacProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "workbuddy-new.json")
	t.Setenv("WB2A_LOGIN_AUTH_FILE", path)
	if err := saveLogin(strings.NewReader(`{"access_token":"at","expires_in":60,"uid":"new-1","domain":"www.codebuddy.cn"}`)); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.DeviceProfile.IsMac() || parsed.DeviceID == "" {
		t.Fatalf("new account profile=%+v identity=%q", parsed.DeviceProfile, parsed.DeviceID)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["_device_identity"] == nil || doc["_device_profile"] == nil {
		t.Fatalf("device metadata missing from document: %v", doc)
	}
}

// 坏输入与不属于同一账号的文件都不能静默覆盖。
func TestSaveLoginRefusesUnsafeOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workbuddy-u9.json")
	t.Setenv("WB2A_LOGIN_AUTH_FILE", path)
	other := &auth.Auth{AccessToken: "keep-at", UID: "other", FilePath: path,
		DeviceProfile: device.LegacyWindows("other", "0123456789abcdef0123456789abcdef0123")}
	if err := other.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveLogin(strings.NewReader(`{"access_token":"new","uid":"u9"}`)); err == nil {
		t.Fatal("mismatched uid must be refused")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refused save modified the credential file")
	}
	for _, tc := range []struct{ name, body string }{
		{"empty token", `{"uid":"u9"}`},
		{"empty uid", `{"access_token":"at"}`},
		{"bad json", `{`},
	} {
		if err := saveLogin(strings.NewReader(tc.body)); err == nil {
			t.Errorf("%s must be refused", tc.name)
		}
	}
}

// 未显式指定路径时按服务端目录约定落盘。
func TestSaveLoginDefaultPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WB2A_LOGIN_AUTH_FILE", "")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	uid := "uid-default-path"
	if err := saveLogin(strings.NewReader(`{"access_token":"at","expires_in":60,"uid":"` + uid + `"}`)); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "auths", "workbuddy-"+uid+".json")); err != nil {
		t.Fatalf("default path not used: %v", err)
	}
}
