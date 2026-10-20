package metricsstore

import "workbuddy2api/internal/responsehistory"

// Responses shares the configured PostgreSQL pool, with independent tables,
// retention, budgets and compression. SQLite uses its own responses.db instead.
func (s *PostgresStore) OpenResponseHistoryStore(p responsehistory.Policy, legacy responsehistory.LegacyStore) (*responsehistory.SQLStore, error) {
	return responsehistory.OpenSQL(s.db, true, p, legacy)
}
