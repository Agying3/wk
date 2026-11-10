package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/device"
)

func getAccountDetails(t *testing.T, h *Handler, uid, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/admin/account/"+uid+"/details", nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAccountDetailsStoredProfilesWhitelist(t *testing.T) {
	id, mac, err := device.NewMac()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []device.Profile{mac, device.LegacyWindows("u", id)} {
		h, _ := testTransferHandler(t)
		a := &auth.Auth{UID: "u", Nickname: "offline", AccessToken: "offline-secret-at", RefreshToken: "offline-secret-rt", Domain: "www.codebuddy.cn", EnterpriseID: "ent", ExpiresAt: 1893456000, DeviceID: id, DeviceProfile: profile, ImportDisabled: true, FilePath: "/private/credential.json", Authority: auth.Authority{AccountType: "ultimate", OneIDAccountID: "oneid", IDP: "provider", DepartmentFullName: "department", TokenType: "Bearer"}}
		h.cfg.Pool.Add(a)
		h.cfg.Pool.Enable(a.UID)
		w := getAccountDetails(t, h, "u", "admin-key")
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("details request failed")
		}
		for _, secret := range []string{a.AccessToken, a.RefreshToken, a.FilePath, id, profile.MachineID, profile.WebMachineID, profile.MPMachineID, profile.SessionID, profile.CLISessionID} {
			if secret != "" && strings.Contains(w.Body.String(), secret) {
				t.Fatal("details exposed a credential, path or complete identifier")
			}
		}
		var got struct {
			Domain       string `json:"domain"`
			AccountType  string `json:"account_type"`
			EnterpriseID string `json:"enterprise_id"`
			Credential   struct {
				HasAccess      bool  `json:"has_access_token"`
				HasRefresh     bool  `json:"has_refresh_token"`
				Expiry         int64 `json:"expires_at"`
				ImportDisabled bool  `json:"import_disabled"`
			} `json:"credential"`
			Device accountDeviceDetails `json:"device"`
			Status struct {
				Disabled bool `json:"disabled"`
			} `json:"status"`
		}
		if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Device.ProfileSource != "stored" || got.Device.Profile.Platform != profile.Platform || got.Device.Profile.CPUCores != profile.CPUCores || got.Device.Profile.MemoryGiB != profile.MemoryGiB || !got.Device.IdentityPresent || got.AccountType != "ultimate" || got.EnterpriseID != "ent" || got.Domain != "www.codebuddy.cn" {
			t.Fatal("profile/authority fields missing")
		}
		if !got.Credential.HasAccess || !got.Credential.HasRefresh || got.Credential.Expiry != 1893456000 || !got.Credential.ImportDisabled || got.Status.Disabled {
			t.Fatal("credential flags confused with current runtime state")
		}
		var full map[string]json.RawMessage
		json.Unmarshal(w.Body.Bytes(), &full)
		var runtime map[string]json.RawMessage
		json.Unmarshal(full["status"], &runtime)
		for _, field := range []string{"success_count", "err_total", "risk_strikes", "cool_remaining_sec", "capacity_used", "cycle_capacity_used"} {
			if string(runtime[field]) != "0" {
				t.Fatal("known zero was omitted from details")
			}
		}
	}
}

func TestAccountDetailsLegacyReadDoesNotInitializeOrSave(t *testing.T) {
	h, _ := testTransferHandler(t)
	if err := os.MkdirAll(h.cfg.AuthDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.cfg.AuthDir, "workbuddy-u.json")
	raw := []byte(`{"uid":"u","accessToken":"offline-at","expiresAt":0,"domain":"www.codebuddy.cn","vendor_secret":"never-return-vendor-secret"}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = path
	h.cfg.Pool.Add(a)
	for range 2 {
		w := getAccountDetails(t, h, "u", "admin-key")
		var got struct {
			Device accountDeviceDetails `json:"device"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Device.ProfileSource != "legacy_derived" || got.Device.Profile.Platform != "windows" || got.Device.IdentityPresent {
			t.Fatal("legacy device source is inaccurate")
		}
		if strings.Contains(w.Body.String(), "never-return-vendor-secret") {
			t.Fatal("unrecognized credential data escaped projection")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) || a.Snapshot().DeviceID != "" || a.Snapshot().DeviceProfile.Version != 0 {
		t.Fatal("viewing details modified device or disk")
	}
}

func TestAccountDetailsAdminOnlyAndMissingAccount(t *testing.T) {
	h, groupKey, _ := newGroupTestHandler(t)
	for _, key := range []string{"", groupKey} {
		if w := getAccountDetails(t, h, "u1", key); w.Code != 401 {
			t.Fatal("non-admin accessed account details")
		}
	}
	if w := getAccountDetails(t, h, "missing", "admin-key"); w.Code != 404 {
		t.Fatalf("missing account status=%d", w.Code)
	}
	empty := NewHandler(Config{APIKey: "admin-key"})
	if w := getAccountDetails(t, empty, "u", "admin-key"); w.Code != 503 {
		t.Fatal("missing pool not reported")
	}
}

func TestAccountDisplayDomainDropsUserInfoAndURLSecrets(t *testing.T) {
	for raw, want := range map[string]string{"www.codebuddy.cn": "www.codebuddy.cn", "https://user:offline-password@api.codebuddy.ai/path?token=offline-query": "api.codebuddy.ai", "": "", "http://[invalid": ""} {
		if got := accountDisplayDomain(raw); got != want {
			t.Fatalf("domain projection=%q want=%q", got, want)
		}
	}
}
