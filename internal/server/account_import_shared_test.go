package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/coordination"
)

func TestAccountImportSharedWriteFailureNotSuccess(t *testing.T) {
	dsn := os.Getenv("WB2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB2API_TEST_POSTGRES_DSN is required")
	}
	h, _ := testTransferHandler(t)
	domain := fmt.Sprintf("import-test/%d", time.Now().UnixNano())
	store, err := coordination.OpenPostgres(context.Background(), dsn, domain)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := h.cfg.Pool.EnableShared(context.Background(), store, "offline-owner", func(string) (bool, error) { return true, nil }, true); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A real PostgreSQL UPDATE failure occurs only once the new account is
	// included, after the credential file has been installed. No live DB is used.
	name := fmt.Sprintf("offline_import_failure_%d", time.Now().UnixNano())
	_, err = db.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.domain = TG_ARGV[0] AND NEW.key = 'pool/runtime' AND NEW.value->'Accounts' ? 'official-offline' THEN
  RAISE EXCEPTION 'offline injected import write failure';
 END IF;
 RETURN NEW;
END $$`)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DROP FUNCTION IF EXISTS ` + name + `() CASCADE`)
	_, err = db.Exec(`CREATE TRIGGER ` + name + ` BEFORE UPDATE ON wb2api_coordination_state FOR EACH ROW EXECUTE FUNCTION ` + name + `('` + domain + `')`)
	if err != nil {
		t.Fatal(err)
	}
	w := transferRequest(t, h, "/admin/accounts/import", accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":0`) || !strings.Contains(w.Body.String(), `"pending":true`) || !strings.Contains(w.Body.String(), `"stop":true`) || h.cfg.Pool.SharedError() == nil {
		t.Fatal("shared write failure was reported as success")
	}
	if strings.Contains(w.Body.String(), "offline-at") || strings.Contains(w.Body.String(), "offline injected") {
		t.Fatal("response exposed credential or database error")
	}
	raw, err := os.ReadFile(filepath.Join(h.cfg.AuthDir, "workbuddy-official-offline.json"))
	if err != nil {
		t.Fatal("uncertain commit must retain its only credential")
	}
	a, err := auth.Parse(raw)
	if err != nil || !a.ImportDisabled {
		t.Fatal("retained credential is not quarantined")
	}
	raw, err = store.View(context.Background(), "pool/runtime")
	if err != nil {
		t.Fatal(err)
	}
	var state struct{ Accounts map[string]json.RawMessage }
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if _, exists := state.Accounts["official-offline"]; exists {
		t.Fatal("injected failure did not roll back PostgreSQL write")
	}
}

func TestAccountImportSharedUnavailableDoesNotCreateFile(t *testing.T) {
	dsn := os.Getenv("WB2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB2API_TEST_POSTGRES_DSN is required")
	}
	h, _ := testTransferHandler(t)
	store, err := coordination.OpenPostgres(context.Background(), dsn, fmt.Sprintf("import-unavailable/%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := h.cfg.Pool.EnableShared(context.Background(), store, "offline-owner", func(string) (bool, error) { return true, nil }, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	w := transferRequest(t, h, "/admin/accounts/import", accountImportRequest{Credential: officialImportCredential(), UIDs: []string{"official-offline"}})
	if w.Code != 503 {
		t.Fatal("unavailable authority did not stop import")
	}
	if _, err := os.Stat(h.cfg.AuthDir); !os.IsNotExist(err) {
		t.Fatal("authority failure created credential files")
	}
}
