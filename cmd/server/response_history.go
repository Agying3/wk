package main

import (
	"fmt"
	"log"
	"path/filepath"

	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/responsehistory"
)

func openResponseHistoryStore(cfg *Config, metrics metricsstore.Backend, legacy responsehistory.LegacyStore) (*responsehistory.SQLStore, error) {
	if pg, ok := metrics.(*metricsstore.PostgresStore); ok {
		s, err := pg.OpenResponseHistoryStore(cfg.ResponseHistory, legacy)
		if err == nil {
			return s, nil
		}
		if !cfg.Postgres.FallbackToSQLite {
			return nil, err
		}
		log.Printf("responses history PostgreSQL unavailable: %v; using SQLite", err)
	} else if cfg.Postgres.DSN != "" && !cfg.Postgres.FallbackToSQLite {
		return nil, fmt.Errorf("configured PostgreSQL unavailable and SQLite fallback disabled")
	}
	return responsehistory.OpenSQLite(filepath.Join(filepath.Dir(cfg.StateFile), "responses.db"), cfg.ResponseHistory, legacy)
}
