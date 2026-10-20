package responsehistory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("previous response history is missing or idle-expired")
	ErrCapacity = errors.New("response history cannot fit the configured entry, count or total capacity limits")
)

const leaseDuration = 30 * time.Second

// initializeConversations migrates the earlier per-turn store once, preserving
// payload bytes. Missing ancestors/cycles cannot be restored and are discarded.
func (s *SQLStore) initializeConversations(ctx context.Context) error {
	if s.postgres {
		for _, column := range []string{"conversation_id", "parent_id"} {
			if _, err := s.db.ExecContext(ctx, `ALTER TABLE response_history_records ADD COLUMN IF NOT EXISTS `+column+` TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	} else {
		rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(response_history_records)`)
		if err != nil {
			return err
		}
		columns := map[string]bool{}
		for rows.Next() {
			var cid, notNull, pk int
			var name, kind string
			var def any
			if err = rows.Scan(&cid, &name, &kind, &notNull, &def, &pk); err != nil {
				rows.Close()
				return err
			}
			columns[name] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, column := range []string{"conversation_id", "parent_id"} {
			if !columns[column] {
				if _, err := s.db.ExecContext(ctx, `ALTER TABLE response_history_records ADD COLUMN `+column+` TEXT NOT NULL DEFAULT ''`); err != nil {
					return err
				}
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS response_history_conversation_idx ON response_history_records(conversation_id)`); err != nil {
		return err
	}
	tx, err := s.writer(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM response_history_records WHERE conversation_id=''`).Scan(&pending); err != nil {
		return err
	}
	if pending == 0 {
		return tx.Commit()
	}
	type oldTurn struct {
		id, parent, root  string
		accessed, expires int64
		state             int
	}
	turns := map[string]*oldTurn{}
	rows, err := tx.QueryContext(ctx, `SELECT id,parent_id,conversation_id,accessed_ms,expires_ms FROM response_history_records`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r oldTurn
		if err = rows.Scan(&r.id, &r.parent, &r.root, &r.accessed, &r.expires); err != nil {
			rows.Close()
			return err
		}
		turns[r.id] = &r
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range turns {
		if r.root != "" {
			continue
		}
		var payload []byte
		var codec string
		var size int64
		if err = tx.QueryRowContext(ctx, s.bind(`SELECT payload,codec,packed_bytes FROM response_history_records WHERE id=?`), r.id).Scan(&payload, &codec, &size); err != nil {
			return err
		}
		packed, decodeErr := decodePayload(payload, codec, size)
		var header struct {
			Parent string `json:"parent_id"`
		}
		if decodeErr != nil || json.Unmarshal(packed, &header) != nil {
			r.state = 2
			continue
		}
		r.parent = header.Parent
	}
	now := time.Now().UnixMilli()
	// Iterative walks avoid recursion on long chains and invalidate descendants
	// of an expired/missing ancestor without inventing a new root.
	for _, start := range turns {
		if start.state != 0 {
			continue
		}
		path := []*oldTurn{}
		seen := map[string]bool{}
		current := start
		root := ""
		for current != nil && current.state != 2 && !seen[current.id] && current.expires > now {
			if current.state == 1 || current.root != "" {
				root = current.root
				break
			}
			seen[current.id] = true
			path = append(path, current)
			if current.parent == "" {
				root = current.id
				break
			}
			current = turns[current.parent]
		}
		for _, r := range path {
			if root == "" {
				r.state = 2
			} else {
				r.state = 1
				r.root = root
			}
		}
	}
	for _, r := range turns {
		if r.state == 2 || r.root == "" {
			if _, err = tx.ExecContext(ctx, s.bind(`DELETE FROM response_history_records WHERE id=?`), r.id); err != nil {
				return err
			}
			continue
		}
		if _, err = tx.ExecContext(ctx, s.bind(`UPDATE response_history_records SET conversation_id=?,parent_id=? WHERE id=?`), r.root, r.parent, r.id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, s.bind(`INSERT INTO response_history_conversations VALUES (?,?,?) ON CONFLICT(id) DO UPDATE SET
		 accessed_ms=CASE WHEN excluded.accessed_ms>response_history_conversations.accessed_ms THEN excluded.accessed_ms ELSE response_history_conversations.accessed_ms END,
		 expires_ms=CASE WHEN excluded.expires_ms>response_history_conversations.expires_ms THEN excluded.expires_ms ELSE response_history_conversations.expires_ms END`), r.root, r.accessed, r.expires); err != nil {
			return err
		}
	}
	if err = s.prune(ctx, tx, now); err != nil {
		return err
	}
	return tx.Commit()
}

// All callers hold the policy writer lock, so sizing, lease checks and whole
// conversation deletion are atomic across PostgreSQL replicas.
func (s *SQLStore) pruneConversations(ctx context.Context, tx *sql.Tx, now int64, keep string, strict bool) error {
	p, err := readLimits(ctx, tx)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.bind(`DELETE FROM response_history_leases WHERE expires_ms<=?`), now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.bind(`UPDATE response_history_conversations SET expires_ms=accessed_ms+? WHERE expires_ms>accessed_ms+?`), p.ttl*1000, p.ttl*1000); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, s.bind(`SELECT c.id,c.expires_ms,COUNT(r.id),COALESCE(SUM(r.logical_bytes),0),COALESCE(MAX(r.logical_bytes),0),
	 (SELECT COUNT(*) FROM response_history_leases l WHERE l.conversation_id=c.id AND l.expires_ms>?)
	 FROM response_history_conversations c LEFT JOIN response_history_records r ON r.conversation_id=c.id
	 GROUP BY c.id,c.expires_ms,c.accessed_ms ORDER BY c.accessed_ms,c.id`), now)
	if err != nil {
		return err
	}
	type item struct {
		id                                     string
		expires, count, bytes, largest, leases int64
		deleted                                bool
	}
	var items []item
	var count, total int64
	for rows.Next() {
		var c item
		if err = rows.Scan(&c.id, &c.expires, &c.count, &c.bytes, &c.largest, &c.leases); err != nil {
			rows.Close()
			return err
		}
		items = append(items, c)
		count += c.count
		total += c.bytes
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	remove := func(c *item) error {
		if _, err := tx.ExecContext(ctx, s.bind(`DELETE FROM response_history_records WHERE conversation_id=?`), c.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.bind(`DELETE FROM response_history_conversations WHERE id=?`), c.id); err != nil {
			return err
		}
		c.deleted = true
		count -= c.count
		total -= c.bytes
		return nil
	}
	for i := range items {
		c := &items[i]
		if c.id == keep || c.leases > 0 {
			continue
		}
		if c.expires <= now || c.largest > p.entry || c.bytes > p.bytes || c.count > p.entries || c.count == 0 {
			if err = remove(c); err != nil {
				return err
			}
		}
	}
	for i := range items {
		if count <= p.entries && total <= p.bytes {
			break
		}
		c := &items[i]
		if c.deleted || c.id == keep || c.leases > 0 {
			continue
		}
		if err = remove(c); err != nil {
			return err
		}
	}
	if strict && (count > p.entries || total > p.bytes) {
		return ErrCapacity
	}
	for _, q := range []string{
		`DELETE FROM response_history_image_refs WHERE NOT EXISTS (SELECT 1 FROM response_history_records WHERE id=record_id)`,
		`DELETE FROM response_history_images WHERE NOT EXISTS (SELECT 1 FROM response_history_image_refs WHERE image_hash=hash)`,
	} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, s.bind(`DELETE FROM response_history_migrated WHERE expires_ms<=?`), now)
	return err
}

func (s *SQLStore) prepareConversation(ctx context.Context, tx *sql.Tx, id, parent string, created, expires, size, now int64, p limits, checksum string, renew bool) (string, error) {
	conversation := id
	if parent != "" {
		if parent == id {
			return "", ErrNotFound
		}
		err := tx.QueryRowContext(ctx, s.bind(`SELECT r.conversation_id FROM response_history_records r
		 JOIN response_history_conversations c ON c.id=r.conversation_id WHERE r.id=? AND
		 (c.expires_ms>? OR EXISTS (SELECT 1 FROM response_history_leases l WHERE l.conversation_id=c.id AND l.expires_ms>?))`), parent, now, now).Scan(&conversation)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", err
		}
	}
	var oldConversation, oldChecksum string
	var oldExpiry, oldLeases int64
	err := tx.QueryRowContext(ctx, s.bind(`SELECT r.conversation_id,r.checksum,c.expires_ms,
	 (SELECT COUNT(*) FROM response_history_leases l WHERE l.conversation_id=c.id AND l.expires_ms>?)
	 FROM response_history_records r JOIN response_history_conversations c ON c.id=r.conversation_id WHERE r.id=?`), now, id).Scan(&oldConversation, &oldChecksum, &oldExpiry, &oldLeases)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if oldConversation != "" && oldConversation != conversation {
		return "", fmt.Errorf("response ID belongs to a different conversation")
	}
	if oldConversation != "" && oldExpiry <= now && oldLeases == 0 {
		return "", ErrNotFound
	}
	if oldChecksum != "" && oldChecksum != checksum {
		return "", fmt.Errorf("response ID collision would change immutable history")
	}
	var count, total, largest int64
	if err = tx.QueryRowContext(ctx, s.bind(`SELECT COUNT(*),COALESCE(SUM(logical_bytes),0),COALESCE(MAX(logical_bytes),0)
	 FROM response_history_records WHERE conversation_id=? AND id<>?`), conversation, id).Scan(&count, &total, &largest); err != nil {
		return "", err
	}
	if count+1 > p.entries || total+size > p.bytes || max(largest, size) > p.entry {
		return "", ErrCapacity
	}
	if parent != "" && renew {
		created, expires = now, now+p.ttl*1000
	}
	if _, err = tx.ExecContext(ctx, s.bind(`INSERT INTO response_history_conversations VALUES (?,?,?) ON CONFLICT(id)
	 DO UPDATE SET accessed_ms=CASE WHEN excluded.accessed_ms>response_history_conversations.accessed_ms THEN excluded.accessed_ms ELSE response_history_conversations.accessed_ms END,
	 expires_ms=CASE WHEN excluded.expires_ms>response_history_conversations.expires_ms THEN excluded.expires_ms ELSE response_history_conversations.expires_ms END`), conversation, created, expires); err != nil {
		return "", err
	}
	// Cold compression uses the same successful activity signal for all branches.
	if _, err = tx.ExecContext(ctx, s.bind(`UPDATE response_history_records SET accessed_ms=? WHERE conversation_id=? AND accessed_ms<?`), now, conversation, now); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, s.bind(`UPDATE response_history_images SET accessed_ms=? WHERE hash IN
	 (SELECT image_hash FROM response_history_image_refs f JOIN response_history_records r ON r.id=f.record_id WHERE r.conversation_id=?)`), now, conversation); err != nil {
		return "", err
	}
	return conversation, nil
}

// AcquireResponse protects the entire conversation throughout an upstream call.
// Leases expire after a crashed process and renew independently of idle TTL.
func (s *SQLStore) AcquireResponse(id string) (func(), error) {
	// Lazy Redis import also imports ancestors before attempting to pin the root.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if s.legacy != nil {
		var exists int
		if err := s.db.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM response_history_records WHERE id=?`), id).Scan(&exists); err != nil {
			return nil, err
		}
		if exists == 0 {
			if err := s.importLegacyChain(id); err != nil {
				return nil, err
			}
		}
	}
	tx, err := s.writer(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	var conversation string
	err = tx.QueryRowContext(ctx, s.bind(`SELECT r.conversation_id FROM response_history_records r JOIN response_history_conversations c ON c.id=r.conversation_id
	 WHERE r.id=? AND (c.expires_ms>? OR EXISTS (SELECT 1 FROM response_history_leases l WHERE l.conversation_id=c.id AND l.expires_ms>?))`), id, now, now).Scan(&conversation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(nonce[:])
	if _, err = tx.ExecContext(ctx, s.bind(`INSERT INTO response_history_leases VALUES (?,?,?)`), token, conversation, now+leaseDuration.Milliseconds()); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(leaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := s.db.ExecContext(ctx, s.bind(`UPDATE response_history_leases SET expires_ms=? WHERE token=?`), time.Now().Add(leaseDuration).UnixMilli(), token)
				cancel()
				if err != nil {
					log.Printf("[responses] history lease renewal failed: %v", err)
				}
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(stop)
			<-done
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			tx, err := s.writer(ctx)
			if err != nil {
				log.Printf("[responses] history lease release failed: %v", err)
				return
			}
			defer tx.Rollback()
			if _, err = tx.ExecContext(ctx, s.bind(`DELETE FROM response_history_leases WHERE token=?`), token); err == nil {
				err = s.prune(ctx, tx, time.Now().UnixMilli())
			}
			if err == nil {
				err = tx.Commit()
			}
			if err != nil {
				log.Printf("[responses] history lease cleanup failed: %v", err)
			}
		})
	}, nil
}

func (s *SQLStore) importLegacyChain(id string) error {
	if s.legacy == nil {
		return ErrNotFound
	}
	type turn struct {
		id      string
		raw     []byte
		expires int64
	}
	var chain []turn
	seen := map[string]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p := limits{}
	if err := s.db.QueryRowContext(ctx, `SELECT max_entries,max_bytes,max_entry_bytes FROM response_history_policy WHERE id=1`).Scan(&p.entries, &p.bytes, &p.entry); err != nil {
		return err
	}
	var bytes int64
	for id != "" {
		if seen[id] {
			return ErrNotFound
		}
		seen[id] = true
		if _, err := s.load(id); err == nil {
			break
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var migrated int
		if err := s.db.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM response_history_migrated WHERE id=?`), id).Scan(&migrated); err != nil {
			return err
		}
		if migrated != 0 {
			return ErrNotFound
		}
		raw, ok := s.legacy.LoadResponse(id)
		if !ok {
			return ErrNotFound
		}
		var header struct {
			Parent  string `json:"parent_id"`
			Expires int64  `json:"expires_at_unix_ms"`
		}
		if json.Unmarshal(raw, &header) != nil || header.Expires <= time.Now().UnixMilli() {
			return ErrNotFound
		}
		bytes += int64(len(raw))
		if bytes > p.bytes || int64(len(raw)) > p.entry || int64(len(chain))+1 > p.entries {
			return ErrCapacity
		}
		chain = append(chain, turn{id, raw, header.Expires})
		id = header.Parent
	}
	for i := len(chain) - 1; i >= 0; i-- {
		r := chain[i]
		if _, err := s.persistResponse(r.id, r.raw, time.Until(time.UnixMilli(r.expires)), true); err != nil {
			return err
		}
	}
	return nil
}
