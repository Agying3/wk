package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/device"
	"workbuddy2api/internal/pool"
)

func TestAccountImportDeviceDefaultsAndPreservation(t *testing.T) {
	id, mac, err := device.NewMac()
	if err != nil {
		t.Fatal(err)
	}
	windows := device.LegacyWindows("official-offline", id)
	for _, tc := range []struct {
		name     string
		profile  *device.Profile
		identity string
	}{
		{"missing", nil, ""}, {"identity-only", nil, id},
		{"windows", &windows, id}, {"mac", &mac, id},
		{"windows-profile-only", &windows, ""}, {"mac-profile-only", &mac, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]json.RawMessage
			json.Unmarshal(officialImportCredential(), &doc)
			if tc.identity != "" {
				doc["_device_identity"], _ = json.Marshal(tc.identity)
			}
			if tc.profile != nil {
				raw, _ := json.Marshal(tc.profile)
				var profile map[string]json.RawMessage
				json.Unmarshal(raw, &profile)
				profile["vendor_counter"] = json.RawMessage("9007199254740993")
				doc["_device_profile"], _ = json.Marshal(profile)
			}
			doc["vendor_counter"] = json.RawMessage("9007199254740993")
			raw, _ := json.Marshal(doc)
			h, _ := testTransferHandler(t)
			req := accountImportRequest{Credential: raw, UIDs: []string{"official-offline"}}
			pre := transferRequest(t, h, "/admin/accounts/import/preview", req)
			var preview struct{ Accounts []accountImportRow }
			if json.Unmarshal(pre.Body.Bytes(), &preview) != nil || pre.Code != 200 || len(preview.Accounts) != 1 {
				t.Fatal("preview failed")
			}
			action := "preserve"
			if tc.profile == nil {
				action = "generate_mac"
			}
			if preview.Accounts[0].DeviceAction != action {
				t.Fatal("preview device plan is wrong")
			}
			if _, err := os.Stat(h.cfg.AuthDir); !os.IsNotExist(err) {
				t.Fatal("preview allocated a device on disk")
			}
			w := transferRequest(t, h, "/admin/accounts/import", req)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":1`) {
				t.Fatal("device import failed")
			}
			a := h.cfg.Pool.AuthByUID("official-offline")
			if a == nil || a.DeviceProfile.Validate() != nil || !h.cfg.Pool.IsDisabled(a.UID) {
				t.Fatal("invalid or enabled imported device")
			}
			if tc.profile == nil {
				if !a.DeviceProfile.IsMac() || a.DeviceID == "" {
					t.Fatal("missing profile did not receive Mac defaults")
				}
				if tc.identity != "" && a.DeviceID != tc.identity {
					t.Fatal("existing device identity was replaced")
				}
			} else if a.DeviceProfile != *tc.profile || a.DeviceID != tc.identity {
				t.Fatal("existing device information was replaced")
			}
			stored, err := os.ReadFile(a.FilePath)
			if err != nil {
				t.Fatal(err)
			}
			var actual map[string]json.RawMessage
			json.Unmarshal(stored, &actual)
			if string(actual["vendor_counter"]) != "9007199254740993" {
				t.Fatal("unknown metadata lost numeric precision")
			}
			if tc.profile != nil && !bytes.Equal(doc["_device_profile"], actual["_device_profile"]) {
				t.Fatal("existing profile extension fields changed")
			}
			loaded, err := auth.LoadDir(h.cfg.AuthDir, "all")
			if err != nil || len(loaded) != 1 || loaded[0].DeviceProfile != a.DeviceProfile || loaded[0].DeviceID != a.DeviceID {
				t.Fatal("device changed after restart")
			}
			fresh := pool.New("")
			fresh.SyncToDir(loaded)
			if fresh.Pick() != nil {
				t.Fatal("new device bypassed default quarantine")
			}
		})
	}
}

func TestAccountImportOldEncryptedCredentialGetsMac(t *testing.T) {
	h, _ := testTransferHandler(t)
	b, err := sealAccounts(testTransferDoc(), transferTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	w := transferRequest(t, h, "/admin/accounts/import", accountImportRequest{Bundle: b, Password: transferTestPassword, UIDs: []string{"u1"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":1`) {
		t.Fatal("old encrypted import failed")
	}
	a := h.cfg.Pool.AuthByUID("u1")
	if a == nil || !a.DeviceProfile.IsMac() || a.DeviceID == "" {
		t.Fatal("old encrypted credential did not receive Mac defaults")
	}
}

func TestAccountImportGroupsAndNicknameOptions(t *testing.T) {
	for _, mode := range []string{"source", "replace", "append"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := testTransferHandler(t)
			if err := h.cfg.Groups.CreateWithRegion("selected", "cn"); err != nil {
				t.Fatal(err)
			}
			req := accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}, GroupMode: mode, Nickname: "  新昵称  "}
			want := []string{"default"}
			if mode != "source" {
				req.Groups = []string{" selected ", "selected"}
				want = []string{"selected"}
				if mode == "append" {
					want = []string{"default", "selected"}
				}
			}
			pre := transferRequest(t, h, "/admin/accounts/import/preview", req)
			var preview struct{ Accounts []accountImportRow }
			if pre.Code != 200 || json.Unmarshal(pre.Body.Bytes(), &preview) != nil || !reflect.DeepEqual(preview.Accounts[0].Groups, want) || preview.Accounts[0].Nickname != "新昵称" {
				t.Fatal("preview did not reflect import options")
			}
			w := transferRequest(t, h, "/admin/accounts/import", req)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":1`) || !reflect.DeepEqual(h.cfg.Groups.AccountGroups("official-offline"), want) {
				t.Fatal("import did not apply previewed groups")
			}
			a := h.cfg.Pool.AuthByUID("official-offline")
			if a == nil || a.Nickname != "新昵称" {
				t.Fatal("nickname override not applied")
			}
			before, _ := os.ReadFile(a.FilePath)
			req.GroupMode, req.Groups, req.Nickname = "replace", []string{"default"}, "覆盖尝试"
			w = transferRequest(t, h, "/admin/accounts/import", req)
			after, _ := os.ReadFile(a.FilePath)
			if !strings.Contains(w.Body.String(), `"skipped":1`) || !bytes.Equal(before, after) || !reflect.DeepEqual(h.cfg.Groups.AccountGroups(a.UID), want) {
				t.Fatal("options overwrote a duplicate account")
			}
		})
	}
}

func TestAccountImportOptionsInvalidOrChangedGroups(t *testing.T) {
	h, _ := testTransferHandler(t)
	for _, req := range []accountImportRequest{
		{GroupMode: "enable"}, {GroupMode: "replace"}, {GroupMode: "append", Groups: []string{" "}},
		{GroupMode: "source", Groups: []string{"default"}},
		{Nickname: strings.Repeat("字", 129)}, {Nickname: "nick\ninjected"},
	} {
		req.Credential = officialImportCredential()
		if w := transferRequest(t, h, "/admin/accounts/import/preview", req); w.Code != 400 {
			t.Fatal("invalid options accepted")
		}
	}
	if err := h.cfg.Groups.CreateWithRegion("wrong-region", "global"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing-group", "wrong-region"} {
		req := accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}, GroupMode: "replace", Groups: []string{name}}
		pre := transferRequest(t, h, "/admin/accounts/import/preview", req)
		var preview struct{ Accounts []accountImportRow }
		json.Unmarshal(pre.Body.Bytes(), &preview)
		if pre.Code != 200 || preview.Accounts[0].Issue == "" {
			t.Fatal("invalid explicit group not marked in preview")
		}
		w := transferRequest(t, h, "/admin/accounts/import", req)
		if !strings.Contains(w.Body.String(), `"success":0`) || h.cfg.Pool.AuthByUID("official-offline") != nil {
			t.Fatal("invalid target silently fell back to default")
		}
	}
	if err := h.cfg.Groups.CreateWithRegion("changed-region", "cn"); err != nil {
		t.Fatal(err)
	}
	req := accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}, GroupMode: "replace", Groups: []string{"changed-region"}}
	if w := transferRequest(t, h, "/admin/accounts/import/preview", req); w.Code != 200 {
		t.Fatal("valid preview failed")
	}
	if err := h.cfg.Groups.SetGroupRegion("changed-region", "global"); err != nil {
		t.Fatal(err)
	}
	w := transferRequest(t, h, "/admin/accounts/import", req)
	if !strings.Contains(w.Body.String(), `"success":0`) {
		t.Fatal("apply reused stale preview validation")
	}
	if _, err := os.Stat(filepath.Join(h.cfg.AuthDir, "workbuddy-official-offline.json")); !os.IsNotExist(err) {
		t.Fatal("invalid group created a credential")
	}
}

func TestAccountImportOptionsGroupSaveFailureIsExplicit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only fixture requires an unprivileged user")
	}
	h, dir := testTransferHandler(t)
	if err := h.cfg.Groups.CreateWithRegion("selected", "cn"); err != nil {
		t.Fatal(err)
	}
	groupDir := filepath.Join(dir, "groups")
	if err := os.Chmod(groupDir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(groupDir, 0700)
	w := transferRequest(t, h, "/admin/accounts/import", accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}, GroupMode: "replace", Groups: []string{"selected"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":0`) || !strings.Contains(w.Body.String(), `"pending":true`) || !strings.Contains(w.Body.String(), "分组保存失败") {
		t.Fatal("group save failure was reported as complete success")
	}
	if !h.cfg.Pool.IsDisabled("official-offline") {
		t.Fatal("partial import was enabled")
	}
}

func TestAccountImportOptionsRetainEncryptedSourceAndFlatNickname(t *testing.T) {
	h, _ := testTransferHandler(t)
	if err := h.cfg.Groups.CreateWithRegion("source", "global"); err != nil {
		t.Fatal(err)
	}
	if err := h.cfg.Groups.CreateWithRegion("target", "global"); err != nil {
		t.Fatal(err)
	}
	doc := testTransferDoc()
	req := accountImportRequest{GroupMode: "append", Groups: []string{"target"}, Nickname: "迁移昵称"}
	if err := normalizeImportOptions(&req, &doc); err != nil {
		t.Fatal(err)
	}
	a, err := auth.Parse(doc.Accounts[0].Credential)
	if err != nil || a.Nickname != "迁移昵称" {
		t.Fatal("flat credential nickname override failed")
	}
	got, issue := h.resolveImportGroups(a, []string{"source", "missing"}, req)
	if issue != "" || !reflect.DeepEqual(got, []string{"source", "target"}) {
		t.Fatal("append did not preserve compatible source groups")
	}
	doc.Accounts = append(doc.Accounts, doc.Accounts[0])
	if err := normalizeImportOptions(&req, &doc); err == nil {
		t.Fatal("batch nickname override accepted")
	}
}

func TestAccountImportOriginalJSONTextPreservesMetadata(t *testing.T) {
	h, _ := testTransferHandler(t)
	text := strings.TrimSuffix(string(officialImportCredential()), "}") + `,"vendor_counter":9007199254740993}`
	req := accountImportRequest{CredentialJSON: text, UIDs: []string{"official-offline"}, Nickname: "文本导入"}
	w := transferRequest(t, h, "/admin/accounts/import", req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":1`) {
		t.Fatal("original text import failed")
	}
	a := h.cfg.Pool.AuthByUID("official-offline")
	stored, err := os.ReadFile(a.FilePath)
	if err != nil || !bytes.Contains(stored, []byte(`"vendor_counter":9007199254740993`)) || a.Nickname != "文本导入" {
		t.Fatal("metadata precision lost through text import")
	}
	req.Credential = officialImportCredential()
	if w := transferRequest(t, h, "/admin/accounts/import/preview", req); w.Code != 400 {
		t.Fatal("both original credential representations were accepted")
	}
}
