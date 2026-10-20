package responsehistory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const coldAfter = 15 * time.Minute

// LegacyStore permits lazy migration of unexpired Redis records. New records
// are committed synchronously to SQL and are never mirrored back to Redis.
type LegacyStore interface {
	SaveResponse(string, []byte, time.Duration)
	LoadResponse(string) ([]byte, bool)
}

// SQLStore owns no transcript cache. SQL indexes locate immutable turns and
// content-addressed image bytes; the handler caches only lightweight metadata.
// Every writer locks the singleton policy row, also serializing shared-PG budgets.
type SQLStore struct {
	db       *sql.DB
	postgres bool
	ownDB    bool
	legacy   LegacyStore
	cancel   context.CancelFunc
	done     chan struct{}
	close    sync.Once
}

func OpenSQLite(path string, p Policy, legacy LegacyStore) (*SQLStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, query := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA synchronous=FULL"} {
		if _, err = db.Exec(query); err != nil {
			db.Close()
			return nil, err
		}
	}
	s, err := OpenSQL(db, false, p, legacy)
	if err != nil {
		db.Close()
		return nil, err
	}
	s.ownDB = true
	return s, nil
}

// OpenSQL reuses a PostgreSQL pool; its owner remains responsible for closing it.
func OpenSQL(db *sql.DB, postgres bool, p Policy, legacy LegacyStore) (*SQLStore, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	s := &SQLStore{db: db, postgres: postgres, legacy: legacy, done: make(chan struct{})}
	blob := "BLOB"
	if postgres {
		blob = "BYTEA"
	}
	queries := []string{
		`CREATE TABLE IF NOT EXISTS response_history_policy (id INTEGER PRIMARY KEY, revision BIGINT NOT NULL,
		 ttl_seconds BIGINT NOT NULL, max_entries BIGINT NOT NULL, max_bytes BIGINT NOT NULL, max_entry_bytes BIGINT NOT NULL)`,
		`INSERT INTO response_history_policy VALUES (1,0,3600,1024,64000000,32000000) ON CONFLICT(id) DO NOTHING`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS response_history_records (id TEXT PRIMARY KEY, payload %s NOT NULL,
		 codec TEXT NOT NULL, packed_bytes BIGINT NOT NULL, logical_bytes BIGINT NOT NULL,
		 checksum TEXT NOT NULL, packed_checksum TEXT NOT NULL, image_refs TEXT NOT NULL,
		 created_ms BIGINT NOT NULL, expires_ms BIGINT NOT NULL, accessed_ms BIGINT NOT NULL, tier INTEGER NOT NULL)`, blob),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS response_history_images (hash TEXT PRIMARY KEY, payload %s NOT NULL,
		 codec TEXT NOT NULL, raw_bytes BIGINT NOT NULL, accessed_ms BIGINT NOT NULL, tier INTEGER NOT NULL)`, blob),
		`CREATE TABLE IF NOT EXISTS response_history_image_refs (record_id TEXT NOT NULL, image_hash TEXT NOT NULL,
		 PRIMARY KEY(record_id,image_hash))`,
		`CREATE TABLE IF NOT EXISTS response_history_migrated (id TEXT PRIMARY KEY, expires_ms BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS response_history_conversations (id TEXT PRIMARY KEY, accessed_ms BIGINT NOT NULL, expires_ms BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS response_history_leases (token TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, expires_ms BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS response_history_lease_idx ON response_history_leases(conversation_id,expires_ms)`,
		`CREATE INDEX IF NOT EXISTS response_history_created_idx ON response_history_records(created_ms,id)`,
		`CREATE INDEX IF NOT EXISTS response_history_expires_idx ON response_history_records(expires_ms)`,
		`CREATE INDEX IF NOT EXISTS response_history_cold_idx ON response_history_records(tier,accessed_ms)`,
		`CREATE INDEX IF NOT EXISTS response_history_image_cold_idx ON response_history_images(tier,accessed_ms)`,
		`CREATE INDEX IF NOT EXISTS response_history_image_hash_idx ON response_history_image_refs(image_hash)`,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, query := range queries {
		if _, err := db.ExecContext(ctx, query); err != nil {
			return nil, fmt.Errorf("initialize response history: %w", err)
		}
	}
	if err := s.initializeConversations(ctx); err != nil {
		return nil, err
	}
	if err := s.ConfigureResponses(p); err != nil {
		return nil, err
	}
	workerCtx, stop := context.WithCancel(context.Background())
	s.cancel = stop
	go s.runMaintenance(workerCtx)
	return s, nil
}

func (s *SQLStore) Close() error {
	var err error
	s.close.Do(func() {
		s.cancel()
		<-s.done
		if s.ownDB {
			err = s.db.Close()
		}
	})
	return err
}

func (s *SQLStore) ResponseHistoryOnDisk() bool { return true }

func (s *SQLStore) ResponseHistoryStorage() string {
	if s.postgres {
		return "postgres"
	}
	return "sqlite"
}

func (s *SQLStore) bind(query string) string {
	if !s.postgres {
		return query
	}
	n := 0
	var b strings.Builder
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *SQLStore) writer(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE response_history_policy SET revision=revision+1 WHERE id=1`); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

type limits struct {
	ttl, entries, bytes, entry int64
}

func readLimits(ctx context.Context, tx *sql.Tx) (limits, error) {
	var p limits
	err := tx.QueryRowContext(ctx, `SELECT ttl_seconds,max_entries,max_bytes,max_entry_bytes FROM response_history_policy WHERE id=1`).Scan(&p.ttl, &p.entries, &p.bytes, &p.entry)
	return p, err
}

func (s *SQLStore) ConfigureResponses(p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.writer(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, s.bind(`UPDATE response_history_policy SET ttl_seconds=?,max_entries=?,max_bytes=?,max_entry_bytes=? WHERE id=1`), p.TTLSeconds, p.MaxEntries, p.MaxBytes(), p.MaxEntryBytes()); err != nil {
		return err
	}
	if err = s.prune(ctx, tx, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLStore) prune(ctx context.Context, tx *sql.Tx, now int64) error {
	return s.pruneConversations(ctx, tx, now, "", false)
}

// PersistResponse returns only after commit. An oversized/expired turn is rejected
// before evicting anything. The budget always counts original serialized bytes.
func (s *SQLStore) PersistResponse(id string, raw []byte, ttl time.Duration) (bool, error) {
	return s.persistResponse(id, raw, ttl, false)
}

func (s *SQLStore) persistResponse(id string, raw []byte, ttl time.Duration, migrating bool) (bool, error) {
	var header struct {
		Created int64  `json:"created_at_unix_ms"`
		Expires int64  `json:"expires_at_unix_ms"`
		Parent  string `json:"parent_id"`
	}
	if id == "" || json.Unmarshal(raw, &header) != nil || header.Created <= 0 || header.Expires <= 0 || ttl <= 0 {
		return false, nil
	}
	now := time.Now().UnixMilli()
	// Reject before image extraction/compression too (policy is rechecked in TX).
	var maxBytes, maxEntry int64
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.db.QueryRowContext(ctx, `SELECT max_bytes,max_entry_bytes FROM response_history_policy WHERE id=1`).Scan(&maxBytes, &maxEntry); err != nil {
		return false, err
	}
	if int64(len(raw)) > maxBytes || int64(len(raw)) > maxEntry {
		return false, ErrCapacity
	}
	if header.Expires <= now {
		return false, ErrNotFound
	}
	packed, refs, images := splitImages(raw)
	payload, codec, err := encodePayload(packed, false)
	if err != nil {
		return false, err
	}
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		return false, err
	}
	tx, err := s.writer(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if migrating {
		var seen int
		if err = tx.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM response_history_migrated WHERE id=?`), id).Scan(&seen); err != nil {
			return false, err
		}
		if seen != 0 {
			return false, nil
		}
		if _, err = tx.ExecContext(ctx, s.bind(`INSERT INTO response_history_migrated VALUES (?,?)`), id, header.Expires); err != nil {
			return false, err
		}
	}
	p, err := readLimits(ctx, tx)
	if err != nil {
		return false, err
	}
	now = time.Now().UnixMilli()
	expires := min(header.Expires, header.Created+p.ttl*1000, time.Now().Add(ttl).UnixMilli())
	if int64(len(raw)) > p.bytes || int64(len(raw)) > p.entry || expires <= time.Now().UnixMilli() {
		if expires <= time.Now().UnixMilli() {
			return false, ErrNotFound
		}
		return false, ErrCapacity
	}
	rawHash := digest(raw)
	conversation, err := s.prepareConversation(ctx, tx, id, header.Parent, header.Created, expires, int64(len(raw)), now, p, rawHash, !migrating)
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, s.bind(`DELETE FROM response_history_image_refs WHERE record_id=?`), id); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, s.bind(`INSERT INTO response_history_records
	 (id,payload,codec,packed_bytes,logical_bytes,checksum,packed_checksum,image_refs,created_ms,expires_ms,accessed_ms,tier,conversation_id,parent_id)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?,0,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,codec=excluded.codec,
	 packed_bytes=excluded.packed_bytes,logical_bytes=excluded.logical_bytes,checksum=excluded.checksum,
	 packed_checksum=excluded.packed_checksum,image_refs=excluded.image_refs,created_ms=excluded.created_ms,
	 expires_ms=excluded.expires_ms,accessed_ms=excluded.accessed_ms,tier=0,conversation_id=excluded.conversation_id,parent_id=excluded.parent_id`),
		id, payload, codec, len(packed), len(raw), rawHash, digest(packed), string(refsJSON), header.Created, expires, now, conversation, header.Parent)
	if err != nil {
		return false, err
	}
	for hash, image := range images {
		if _, err = tx.ExecContext(ctx, s.bind(`INSERT INTO response_history_images (hash,payload,codec,raw_bytes,accessed_ms,tier)
		 VALUES (?,?,'raw',?,?,0) ON CONFLICT(hash) DO UPDATE SET accessed_ms=CASE WHEN response_history_images.accessed_ms>excluded.accessed_ms THEN response_history_images.accessed_ms ELSE excluded.accessed_ms END`), hash, image, len(image), now); err != nil {
			return false, err
		}
		if _, err = tx.ExecContext(ctx, s.bind(`INSERT INTO response_history_image_refs VALUES (?,?)`), id, hash); err != nil {
			return false, err
		}
	}
	if err = s.pruneConversations(ctx, tx, time.Now().UnixMilli(), conversation, true); err != nil {
		return false, err
	}
	var exists int
	if err = tx.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM response_history_records WHERE id=?`), id).Scan(&exists); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return exists == 1, nil
}

func (s *SQLStore) SaveResponse(id string, raw []byte, ttl time.Duration) {
	if _, err := s.PersistResponse(id, raw, ttl); err != nil {
		log.Printf("[responses] history persistence failed: %v", err)
	}
}

func (s *SQLStore) LoadResponse(id string) ([]byte, bool) {
	raw, err := s.ReadResponse(id)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			log.Printf("[responses] history read failed: %v", err)
		}
		return nil, false
	}
	return raw, true
}

func (s *SQLStore) ReadResponse(id string) ([]byte, error) {
	raw, err := s.load(id)
	if errors.Is(err, sql.ErrNoRows) && s.legacy != nil {
		if importErr := s.importLegacyChain(id); importErr != nil {
			return nil, importErr
		}
		raw, err = s.load(id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return raw, err
}

func (s *SQLStore) load(id string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	options := &sql.TxOptions{ReadOnly: true}
	if s.postgres {
		options.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.db.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var payload []byte
	var codec, checksum, packedChecksum, refsJSON string
	var packedSize, size int64
	now := time.Now().UnixMilli()
	err = tx.QueryRowContext(ctx, s.bind(`SELECT payload,codec,packed_bytes,logical_bytes,checksum,packed_checksum,image_refs
	 FROM response_history_records r JOIN response_history_conversations c ON c.id=r.conversation_id
	 CROSS JOIN response_history_policy p WHERE r.id=? AND p.id=1
	 AND (c.expires_ms>? OR EXISTS (SELECT 1 FROM response_history_leases l WHERE l.conversation_id=c.id AND l.expires_ms>?))
	 AND ((logical_bytes<=p.max_entry_bytes AND logical_bytes<=p.max_bytes)
	 OR EXISTS (SELECT 1 FROM response_history_leases l WHERE l.conversation_id=c.id AND l.expires_ms>?))`), id, now, now, now).
		Scan(&payload, &codec, &packedSize, &size, &checksum, &packedChecksum, &refsJSON)
	if err != nil {
		return nil, err
	}
	if packedSize > size || size < 0 {
		return nil, fmt.Errorf("invalid history size")
	}
	packed, err := decodePayload(payload, codec, packedSize)
	if err != nil || digest(packed) != packedChecksum {
		return nil, fmt.Errorf("history document integrity check failed")
	}
	var refs []imageRef
	if err = json.Unmarshal([]byte(refsJSON), &refs); err != nil {
		return nil, fmt.Errorf("invalid history image manifest")
	}
	images := make(map[string][]byte)
	rows, err := tx.QueryContext(ctx, s.bind(`SELECT i.hash,i.payload,i.codec,i.raw_bytes FROM response_history_images i
	 JOIN response_history_image_refs r ON r.image_hash=i.hash WHERE r.record_id=?`), id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var hash, imageCodec string
		var imagePayload []byte
		var imageSize int64
		if err = rows.Scan(&hash, &imagePayload, &imageCodec, &imageSize); err == nil {
			if imageSize < 0 || imageSize > size {
				err = fmt.Errorf("invalid history image size")
			} else {
				images[hash], err = decodePayload(imagePayload, imageCodec, imageSize)
			}
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	raw, err := joinImages(packed, refs, images, size, checksum)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	// Only a successfully committed continuation renews LRU and idle TTL.
	return raw, nil
}

func (s *SQLStore) runMaintenance(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.maintain(ctx, time.Now()); err != nil && ctx.Err() == nil {
				log.Printf("[responses] history maintenance failed: %v", err)
			}
		}
	}
}

// At most four documents and four images per minute. Compression runs outside
// transactions; conditional replacement cannot overwrite a recently accessed turn.
func (s *SQLStore) maintain(parent context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	tx, err := s.writer(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.prune(ctx, tx, now.UnixMilli()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	cutoff := now.Add(-coldAfter).UnixMilli()
	for _, table := range []string{"response_history_records", "response_history_images"} {
		key, size, checksum := "id", "packed_bytes", "packed_checksum"
		if table == "response_history_images" {
			key, size, checksum = "hash", "raw_bytes", "hash"
		}
		// Queue identifiers only, so a batch cannot hold four full image bodies.
		rows, err := s.db.QueryContext(ctx, s.bind(`SELECT `+key+`,`+checksum+` FROM `+table+` WHERE tier=0 AND accessed_ms<=? ORDER BY accessed_ms LIMIT 4`), cutoff)
		if err != nil {
			return err
		}
		type candidate struct {
			id, checksum string
		}
		var candidates []candidate
		for rows.Next() {
			var c candidate
			if err = rows.Scan(&c.id, &c.checksum); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, c := range candidates {
			if err := ctx.Err(); err != nil {
				return err
			}
			var current []byte
			var currentCodec string
			var rawSize int64
			err := s.db.QueryRowContext(ctx, s.bind(`SELECT payload,codec,`+size+` FROM `+table+` WHERE `+key+`=? AND `+checksum+`=? AND tier=0 AND accessed_ms<=?`), c.id, c.checksum, cutoff).Scan(&current, &currentCodec, &rawSize)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			raw, err := decodePayload(current, currentCodec, rawSize)
			if err != nil || digest(raw) != c.checksum {
				return fmt.Errorf("cold history integrity check failed")
			}
			payload, codec, err := encodePayload(raw, true)
			if err != nil {
				return err
			}
			if len(payload) >= len(current) {
				payload, codec = current, currentCodec
			}
			// Verify new bytes before committing the replacement.
			restored, err := decodePayload(payload, codec, rawSize)
			if err != nil || digest(restored) != c.checksum {
				return fmt.Errorf("cold history verification failed")
			}
			_, err = s.db.ExecContext(ctx, s.bind(`UPDATE `+table+` SET payload=?,codec=?,tier=1 WHERE `+key+`=? AND `+checksum+`=? AND tier=0 AND accessed_ms<=?`), payload, codec, c.id, c.checksum, cutoff)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
