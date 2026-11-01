package server

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

func TestAccountAuthorityPersistAndRelogin(t *testing.T) {
	h := newTestHandler(t, Config{AuthDir: t.TempDir(), Pool: pool.New(""), Region: "cn"})
	metadata := auth.Authority{AccountType: "ultimate", OneIDAccountID: "oid", IDP: "provider", DepartmentFullName: "department", TokenType: "Bearer"}
	cred := accountCredential{Authority: metadata, UID: "u", AccessToken: "fake-at", RefreshToken: "fake-rt", Domain: "www.codebuddy.cn"}
	if _, _, err := h.persistAccount(cred, "cn"); err != nil {
		t.Fatal(err)
	}
	before := h.cfg.Pool.AuthByUID("u").Device()
	for _, access := range []string{"fake-at", "rotated-fake-at"} {
		cred.AccessToken = access
		if _, _, err := h.persistAccount(cred, "cn"); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(h.cfg.AuthDir, "workbuddy-u.json"))
		if err != nil {
			t.Fatal(err)
		}
		a, err := auth.Parse(raw)
		if err != nil || a.Snapshot().Authority != metadata || a.Device() != before || h.cfg.Pool.AuthByUID("u").Snapshot().Authority != metadata {
			t.Fatalf("persisted metadata/device mismatch: %v", err)
		}
	}
}
