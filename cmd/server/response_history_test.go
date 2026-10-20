package main

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/responsehistory"
)

func TestOpenResponseHistoryStorageSelection(t *testing.T) {
	var cfg Config
	cfg.ResponseHistory = responsehistory.Default()
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	s, err := openResponseHistoryStore(&cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.ResponseHistoryStorage() != "sqlite" {
		t.Fatal("missing default disk persistence")
	}
	s.Close()
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.StateFile), "responses.db")); err != nil {
		t.Fatal(err)
	}
	cfg.Postgres.DSN = "configured-but-unavailable"
	if s, err := openResponseHistoryStore(&cfg, nil, nil); err == nil || s != nil {
		t.Fatal("unapproved database fallback")
	}
	cfg.Postgres.FallbackToSQLite = true
	s, err = openResponseHistoryStore(&cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}
