package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

func officialImportCredential() json.RawMessage {
	return json.RawMessage(`{"auth":{"accessToken":"offline-at","refreshToken":"offline-rt","expiresAt":1893456000000,"realm":"cn","domain":"www.codebuddy.cn"},"account":{"uid":"official-offline","enterpriseId":"","nickname":"Offline account"}}`)
}

func TestAccountImportOfficialPreviewApplyAndDuplicate(t *testing.T) {
	h, _ := testTransferHandler(t)
	req := accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}}
	pre := transferRequest(t, h, "/admin/accounts/import/preview", req)
	if pre.Code != 200 || pre.Header().Get("Cache-Control") != "no-store" || strings.Contains(pre.Body.String(), "offline-at") || strings.Contains(pre.Body.String(), "offline-rt") {
		t.Fatal("preview failed or leaked credentials")
	}
	if h.cfg.Pool.AuthByUID("official-offline") != nil || h.cfg.Region != "cn" {
		t.Fatal("preview changed runtime state")
	}
	if _, err := os.Stat(h.cfg.AuthDir); !os.IsNotExist(err) {
		t.Fatal("preview wrote to the credential directory")
	}
	w := transferRequest(t, h, "/admin/accounts/import", req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":1`) || strings.Contains(w.Body.String(), "offline-at") {
		t.Fatal("official import failed or leaked credentials")
	}
	a := h.cfg.Pool.AuthByUID("official-offline")
	if a == nil || a.Snapshot().ExpiresAt != 1893456000 || !h.cfg.Pool.IsDisabled(a.UID) || h.cfg.Pool.Pick() != nil {
		t.Fatal("official import failed to normalize expiry or quarantine account")
	}
	before, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	pre = transferRequest(t, h, "/admin/accounts/import/preview", req)
	if !strings.Contains(pre.Body.String(), `"exists":true`) {
		t.Fatal("duplicate not shown in preview")
	}
	req.Credential = bytes.ReplaceAll(req.Credential, []byte("offline-at"), []byte("replacement"))
	w = transferRequest(t, h, "/admin/accounts/import", req)
	after, _ := os.ReadFile(a.FilePath)
	if !strings.Contains(w.Body.String(), `"skipped":1`) || !bytes.Equal(before, after) {
		t.Fatal("duplicate overwrote an existing credential")
	}
	loaded, err := auth.LoadDir(h.cfg.AuthDir, "all")
	if err != nil || len(loaded) != 1 || !loaded[0].ImportDisabled {
		t.Fatal("quarantine marker missing")
	}
	fresh := pool.New("")
	fresh.SyncToDir(loaded)
	if fresh.Pick() != nil {
		t.Fatal("import became routable after losing pool state")
	}
}

func TestAccountImportOfficialInvalidInputs(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"accessToken":"fake","uid":"flat"}`,
		`{"auth":null,"account":{"uid":"u"}}`,
		`{"auth":{"accessToken":"fake"},"account":null}`,
		`{"auth":{"accessToken":"fake"},"account":{"uid":"../escape"}}`,
		`{"auth":{"accessToken":"fake\r\nheader"},"account":{"uid":"u"}}`,
		`{"auth":{"accessToken":""},"account":{"uid":"u"}}`,
	} {
		h, _ := testTransferHandler(t)
		w := transferRequest(t, h, "/admin/accounts/import/preview", accountImportRequest{Credential: json.RawMessage(raw)})
		if w.Code != 400 {
			t.Fatal("invalid official input accepted")
		}
	}
	h, _ := testTransferHandler(t)
	b, _ := sealAccounts(testTransferDoc(), transferTestPassword)
	w := transferRequest(t, h, "/admin/accounts/import/preview", accountImportRequest{Credential: officialImportCredential(), Bundle: b, Password: transferTestPassword})
	if w.Code != 400 {
		t.Fatal("mixed import modes accepted")
	}
	for _, req := range []accountImportRequest{
		{Credential: officialImportCredential()},
		{Credential: officialImportCredential(), UIDs: []string{"foreign"}},
	} {
		if w := transferRequest(t, h, "/admin/accounts/import", req); w.Code != 400 {
			t.Fatal("selection validation bypassed")
		}
	}
}

func TestAccountImportReadOnlyConfigAlreadyCompatible(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only fixture requires an unprivileged user")
	}
	for _, region := range []string{"all", "cn"} {
		t.Run(region, func(t *testing.T) {
			h, dir := testTransferHandler(t)
			h.cfg.Region = region
			configDir := filepath.Join(dir, "read-only-config")
			if err := os.Mkdir(configDir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(configDir, "config.json")
			content := []byte(fmt.Sprintf(`{"region":%q,"preserve":true}`, region))
			if err := os.WriteFile(path, content, 0400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(configDir, 0500); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(configDir, 0700)
			h.cfg.ConfigPath = path
			w := transferRequest(t, h, "/admin/accounts/import", accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}})
			after, _ := os.ReadFile(path)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":1`) || !bytes.Equal(content, after) || h.cfg.Region != region {
				t.Fatal("compatible import attempted an unnecessary config write")
			}
		})
	}
}

func TestAccountTransferRoundtrip101Groups(t *testing.T) {
	h, _ := testTransferHandler(t)
	h.cfg.Pool.Add(&auth.Auth{UID: "offline-many-groups", AccessToken: "fake", Domain: "www.codebuddy.cn"})
	names := []string{}
	for i := 0; i < 101; i++ {
		name := fmt.Sprintf("offline-%03d", i)
		if err := h.cfg.Groups.Create(name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := h.cfg.Groups.SetAccountGroups("offline-many-groups", names); err != nil {
		t.Fatal(err)
	}
	w := transferRequest(t, h, "/admin/accounts/export", map[string]any{"uids": []string{"offline-many-groups"}, "password": transferTestPassword})
	if w.Code != 200 {
		t.Fatal("many-group export failed")
	}
	var b accountTransferBundle
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	doc, err := openAccounts(b, transferTestPassword)
	if err != nil || len(doc.Accounts[0].Groups) != 101 {
		t.Fatal("export/import membership limits differ")
	}
	target, _ := testTransferHandler(t)
	for _, name := range names {
		if err := target.cfg.Groups.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	req := accountImportRequest{Bundle: b, Password: transferTestPassword, UIDs: []string{"offline-many-groups"}}
	if pre := transferRequest(t, target, "/admin/accounts/import/preview", req); pre.Code != 200 {
		t.Fatal("many-group preview failed")
	}
	w = transferRequest(t, target, "/admin/accounts/import", req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":1`) || len(target.cfg.Groups.AccountGroups("offline-many-groups")) != 101 {
		t.Fatal("many-group import lost memberships")
	}
}

func TestAccountTransferExportRejectsUnimportableCredentials(t *testing.T) {
	for _, a := range []*auth.Auth{
		{UID: "../invalid", AccessToken: "fake"},
		{UID: "offline-large", AccessToken: "fake", Nickname: strings.Repeat("n", accountCredentialBytes)},
	} {
		h, _ := testTransferHandler(t)
		h.cfg.Pool.Add(a)
		w := transferRequest(t, h, "/admin/accounts/export", map[string]any{"uids": []string{a.UID}, "password": transferTestPassword})
		if w.Code != 400 {
			t.Fatal("export produced a credential rejected by import")
		}
	}
}

func TestAccountImportPersistenceFailureNotSuccess(t *testing.T) {
	h, _ := testTransferHandler(t)
	h.cfg.Pool.SetStatePersister(&batchPersister{err: errors.New("offline persistence failure")})
	w := transferRequest(t, h, "/admin/accounts/import", accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":0`) || !strings.Contains(w.Body.String(), `"pending":true`) || !strings.Contains(w.Body.String(), `"stop":true`) {
		t.Fatal("persistence failure was reported as successful import")
	}
	if !h.cfg.Pool.IsDisabled("official-offline") {
		t.Fatal("failure activated imported account")
	}
}

func TestAccountTransferCiphertextSizeBoundary(t *testing.T) {
	doc := testTransferDoc()
	doc.Accounts[0].Groups = []string{""}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc.Accounts[0].Groups[0] = strings.Repeat("x", accountTransferPlainBytes-len(raw))
	b, err := sealAccounts(doc, transferTestPassword)
	if err != nil || len(b.Ciphertext) != accountTransferPlainBytes+16 {
		t.Fatal("plaintext limit was not sealed with GCM overhead")
	}
	if _, err := openAccounts(b, transferTestPassword); err != nil {
		t.Fatal("exported boundary document cannot be imported")
	}
}
