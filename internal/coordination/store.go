// Package coordination serializes overlapping local release processes through
// one PostgreSQL authority. It deliberately has no file or memory fallback.
package coordination

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct {
	db     *sql.DB
	domain string
}

func OpenPostgres(ctx context.Context, dsn, domain string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" || strings.TrimSpace(domain) == "" || len(domain) > 200 {
		return nil, errors.New("overlap coordination requires a PostgreSQL DSN and a domain")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open coordination database: %w", err)
	}
	s := &Store{db: db, domain: domain}
	if err = db.PingContext(ctx); err == nil {
		_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS wb2api_coordination_state (
			domain TEXT NOT NULL, key TEXT NOT NULL, value JSONB NOT NULL DEFAULT 'null',
			revision BIGINT NOT NULL DEFAULT 0, PRIMARY KEY(domain, key))`)
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize coordination database: %w", err)
	}
	return s, nil
}

func (s *Store) Domain() string { return s.domain }
func (s *Store) Close() error   { return s.db.Close() }

// Tx owns a row lock until Commit or Rollback. Never hold it across an upstream
// generation; credential refresh has its own independent row and deadline.
type Tx struct {
	tx          *sql.Tx
	domain, key string
	value       json.RawMessage
	ctx         context.Context
}

func (s *Store) Begin(ctx context.Context, key string) (*Tx, error) {
	if strings.TrimSpace(key) == "" || len(key) > 500 {
		return nil, errors.New("invalid coordination key")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `INSERT INTO wb2api_coordination_state(domain,key) VALUES($1,$2) ON CONFLICT DO NOTHING`, s.domain, key); err != nil {
		return nil, err
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT value FROM wb2api_coordination_state WHERE domain=$1 AND key=$2 FOR UPDATE`, s.domain, key).Scan(&raw); err != nil {
		return nil, err
	}
	ok = true
	return &Tx{tx: tx, domain: s.domain, key: key, value: append(json.RawMessage(nil), raw...), ctx: ctx}, nil
}

func (t *Tx) Value() json.RawMessage { return append(json.RawMessage(nil), t.value...) }
func (t *Tx) Rollback() error        { return t.tx.Rollback() }
func (t *Tx) Write(value json.RawMessage) error {
	if !json.Valid(value) {
		return errors.New("invalid coordination JSON")
	}
	if _, err := t.tx.ExecContext(t.ctx, `UPDATE wb2api_coordination_state SET value=$3::jsonb,revision=revision+1 WHERE domain=$1 AND key=$2`, t.domain, t.key, string(value)); err != nil {
		return err
	}
	t.value = append(json.RawMessage(nil), value...)
	return nil
}
func (t *Tx) CommitCurrent() error { return t.tx.Commit() }
func (t *Tx) Commit(value json.RawMessage) error {
	if err := t.Write(value); err != nil {
		_ = t.tx.Rollback()
		return err
	}
	return t.CommitCurrent()
}

func (s *Store) Update(ctx context.Context, key string, fn func(json.RawMessage) (json.RawMessage, error)) error {
	tx, err := s.Begin(ctx, key)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	next, err := fn(tx.Value())
	if err != nil {
		return err
	}
	if next == nil {
		next = json.RawMessage("null")
	}
	return tx.Commit(next)
}

func (s *Store) View(ctx context.Context, key string) (json.RawMessage, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM wb2api_coordination_state WHERE domain=$1 AND key=$2`, s.domain, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return json.RawMessage("null"), nil
	}
	if err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), raw...), nil
}
