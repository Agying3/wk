package responsehistory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func linkedTurn(text, parent string, created time.Time) []byte {
	var doc map[string]any
	_ = json.Unmarshal(testTurn(text, "AA==", created), &doc)
	if parent != "" {
		doc["parent_id"] = parent
	}
	raw, _ := json.Marshal(doc)
	return raw
}

func setIdle(t *testing.T, s *SQLStore, conversation string, last time.Time, ttl time.Duration) {
	t.Helper()
	if _, err := s.db.Exec(s.bind(`UPDATE response_history_conversations SET accessed_ms=?,expires_ms=? WHERE id=?`), last.UnixMilli(), last.Add(ttl).UnixMilli(), conversation); err != nil {
		t.Fatal(err)
	}
}

func missing(t *testing.T, s *SQLStore, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, ok := s.LoadResponse(id); ok {
			t.Fatalf("unexpected surviving turn %s", id)
		}
	}
}

func conversationConformance(t *testing.T, s *SQLStore) {
	now := time.Now()
	p := Default()
	p.MaxEntries = 4
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	a := linkedTurn("a-root", "", now)
	a1 := linkedTurn("a-child", "a", now)
	a2 := linkedTurn("a-fork", "a", now)
	persistTest(t, s, "a", a)
	persistTest(t, s, "a1", a1)
	b := linkedTurn("b-root", "", now)
	persistTest(t, s, "b", b)
	setIdle(t, s, "a", now.Add(-20*time.Minute), time.Hour)
	setIdle(t, s, "b", now.Add(-10*time.Minute), time.Hour)
	// A successful branch continuation refreshes the root and all its branches.
	persistTest(t, s, "a2", a2)
	c := linkedTurn("c-root", "", now)
	persistTest(t, s, "c", c)
	missing(t, s, "b")
	assertStored(t, s, "a", a)
	assertStored(t, s, "a1", a1)
	assertStored(t, s, "a2", a2)
	// Reading/pinning without a successful append must not keep failed traffic hot.
	before := scalar(t, s, `SELECT accessed_ms FROM response_history_conversations WHERE id='a'`)
	release, err := s.AcquireResponse("a1")
	if err != nil {
		t.Fatal(err)
	}
	assertStored(t, s, "a1", a1)
	release()
	if scalar(t, s, `SELECT accessed_ms FROM response_history_conversations WHERE id='a'`) != before {
		t.Fatal("failed/read-only request renewed activity")
	}
	// LRU evicts the whole older conversation, including siblings.
	setIdle(t, s, "a", now.Add(-time.Minute), time.Hour)
	persistTest(t, s, "d", linkedTurn("d-root", "", now))
	missing(t, s, "a", "a1", "a2")
	if scalar(t, s, `SELECT COUNT(*) FROM response_history_records WHERE parent_id<>''`) != 0 {
		t.Fatal("dangling descendants retained")
	}
	// Re-create a chain whose root's immutable JSON timestamp is already old.
	p.MaxEntries = 6
	p.TTLSeconds = 3600
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	old := linkedTurn("old root", "", now.Add(-30*time.Minute))
	persistTest(t, s, "idle", old)
	persistTest(t, s, "idle-child", linkedTurn("active child", "idle", now))
	p.TTLSeconds = 60
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	assertStored(t, s, "idle", old) // Creation-based TTL would incorrectly reject it.
	setIdle(t, s, "idle", now.Add(-2*time.Minute), time.Minute)
	if err := s.maintain(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	missing(t, s, "idle", "idle-child")
	p.TTLSeconds = 3600
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	missing(t, s, "idle", "idle-child")
}

func TestSQLiteConversationLRUAndIdleTTL(t *testing.T) {
	conversationConformance(t, openTestSQLite(t, Default()))
}

func leaseConformance(t *testing.T, s, other *SQLStore) {
	p := Default()
	p.MaxEntries = 2
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a := linkedTurn("protected root", "", now)
	child := linkedTurn("protected child", "a", now)
	persistTest(t, s, "a", a)
	persistTest(t, s, "a1", child)
	release, err := s.AcquireResponse("a1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	otherRelease, err := other.AcquireResponse("a")
	if err != nil {
		t.Fatal(err)
	}
	defer otherRelease()
	setIdle(t, s, "a", now.Add(-2*time.Hour), time.Hour)
	if err := other.maintain(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	assertStored(t, other, "a", a)
	// Nothing can be evicted while all existing capacity is leased. The new write
	// rolls back, instead of deleting ancestors or exceeding the hard budget.
	if ok, err := other.PersistResponse("b", linkedTurn("new root", "", now), time.Hour); ok || !errors.Is(err, ErrCapacity) {
		t.Fatalf("lease budget rejection: %v %v", ok, err)
	}
	missing(t, other, "b")
	assertStored(t, other, "a1", child)
	// Shrinking policy defers removal of leased conversations until release.
	p.MaxEntries = 1
	if err := other.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	assertStored(t, other, "a", a)
	release()
	assertStored(t, other, "a", a)
	otherRelease()
	missing(t, other, "a", "a1")
	if scalar(t, other, `SELECT COUNT(*) FROM response_history_leases`) != 0 {
		t.Fatal("lease leaked")
	}
	// A crashed process leaves only a bounded lease, not an immortal conversation.
	p.MaxEntries = 2
	if err := other.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	persistTest(t, s, "crash", linkedTurn("crash", "", now))
	setIdle(t, s, "crash", now.Add(-2*time.Hour), time.Hour)
	if _, err := s.db.Exec(s.bind(`INSERT INTO response_history_leases VALUES ('expired-test-lease','crash',?)`), now.Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := other.maintain(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	missing(t, other, "crash")
}

func TestSQLiteConversationLeasesAcrossConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses.db")
	s, err := OpenSQLite(path, Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := OpenSQLite(path, Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	leaseConformance(t, s, other)
}

func TestSQLiteConversationCapacityRollback(t *testing.T) {
	s := openTestSQLite(t, Default())
	p := Default()
	p.MaxStorageMB = 1
	p.MaxEntryMB = 1
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := linkedTurn(strings.Repeat("a", 600000), "", now)
	persistTest(t, s, "root", root)
	persistTest(t, s, "other", linkedTurn("unrelated", "", now))
	before := scalar(t, s, `SELECT SUM(logical_bytes) FROM response_history_records`)
	if ok, err := s.PersistResponse("child", linkedTurn(strings.Repeat("b", 600000), "root", now), time.Hour); ok || !errors.Is(err, ErrCapacity) {
		t.Fatalf("conversation too large: %v %v", ok, err)
	}
	assertStored(t, s, "root", root)
	missing(t, s, "child")
	if scalar(t, s, `SELECT SUM(logical_bytes) FROM response_history_records`) != before {
		t.Fatal("capacity rejection changed unrelated history")
	}
	if _, ok := s.LoadResponse("other"); !ok {
		t.Fatal("rejected append evicted unrelated conversation")
	}
	// Duplicate IDs must not create cycles or rewrite already-sent history.
	if ok, err := s.PersistResponse("root", linkedTurn("rewritten", "root", now), time.Hour); ok || err == nil {
		t.Fatal("self-reference accepted")
	}
	assertStored(t, s, "root", root)
}

func TestSQLiteConversationMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE response_history_records (id TEXT PRIMARY KEY,payload BLOB NOT NULL,codec TEXT NOT NULL,packed_bytes BIGINT NOT NULL,
	 logical_bytes BIGINT NOT NULL,checksum TEXT NOT NULL,packed_checksum TEXT NOT NULL,image_refs TEXT NOT NULL,created_ms BIGINT NOT NULL,
	 expires_ms BIGINT NOT NULL,accessed_ms BIGINT NOT NULL,tier INTEGER NOT NULL)`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	now := time.Now()
	for id, parent := range map[string]string{"root": "", "child": "root", "orphan": "missing", "cycle1": "cycle2", "cycle2": "cycle1"} {
		raw := linkedTurn(id, parent, now)
		if _, err = db.Exec(`INSERT INTO response_history_records VALUES (?,?,'raw',?,?,?,?, 'null',?,?,?,0)`, id, raw, len(raw), len(raw), digest(raw), digest(raw), now.UnixMilli(), now.Add(time.Hour).UnixMilli(), now.UnixMilli()); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := OpenSQLite(path, Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if scalar(t, s, `SELECT COUNT(*) FROM response_history_conversations`) != 1 || scalar(t, s, `SELECT COUNT(*) FROM response_history_records WHERE conversation_id='root'`) != 2 {
		t.Fatal("migration did not group the intact chain")
	}
	missing(t, s, "orphan", "cycle1", "cycle2")
	assertStored(t, s, "root", linkedTurn("root", "", now))
	assertStored(t, s, "child", linkedTurn("child", "root", now))
}

func TestSQLiteLegacyChainMigration(t *testing.T) {
	now := time.Now()
	old := legacyFixture{"old-root": linkedTurn("root", "", now), "old-child": linkedTurn("child", "old-root", now)}
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "responses.db"), Default(), old)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	release, err := s.AcquireResponse("old-child")
	if err != nil {
		t.Fatal(err)
	}
	release()
	assertStored(t, s, "old-root", old["old-root"])
	assertStored(t, s, "old-child", old["old-child"])
	p := Default()
	p.MaxEntries = 1
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	missing(t, s, "old-root", "old-child")
}

func TestSQLiteConversationLeaseRenewal(t *testing.T) {
	s := openTestSQLite(t, Default())
	persistTest(t, s, "renew", linkedTurn("long request", "", time.Now()))
	release, err := s.AcquireResponse("renew")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	initial := scalar(t, s, `SELECT expires_ms FROM response_history_leases`)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if scalar(t, s, `SELECT expires_ms FROM response_history_leases`) > initial+5000 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("long-running request did not renew its lease")
}

func concurrentConversationForks(t *testing.T, s, other *SQLStore) {
	p := Default()
	p.MaxEntries = 5
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	persistTest(t, s, "fork-root", linkedTurn("root", "", time.Now()))
	var wg sync.WaitGroup
	saved := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := s
			if i%2 == 1 {
				db = other
			}
			release, err := db.AcquireResponse("fork-root")
			if err != nil {
				t.Error(err)
				saved <- false
				return
			}
			defer release()
			ok, err := db.PersistResponse(fmt.Sprintf("fork-%d", i), linkedTurn("branch", "fork-root", time.Now()), time.Hour)
			if err != nil && !errors.Is(err, ErrCapacity) {
				t.Error(err)
			}
			saved <- ok
		}(i)
	}
	wg.Wait()
	close(saved)
	n := 0
	for ok := range saved {
		if ok {
			n++
		}
	}
	if n != 4 || scalar(t, s, `SELECT COUNT(*) FROM response_history_records WHERE conversation_id='fork-root'`) != 5 {
		t.Fatalf("concurrent fork budget: %d successful branches", n)
	}
	if _, ok := s.LoadResponse("fork-root"); !ok {
		t.Fatal("concurrent forks deleted their ancestor")
	}
}

func TestSQLiteConcurrentConversationForks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses.db")
	s, err := OpenSQLite(path, Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := OpenSQLite(path, Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	concurrentConversationForks(t, s, other)
}

func TestSQLiteConversationActivitySurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses.db")
	p := Default()
	p.MaxEntries = 3
	s, err := OpenSQLite(path, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	persistTest(t, s, "a", linkedTurn("old root", "", now.Add(-20*time.Minute)))
	persistTest(t, s, "b", linkedTurn("newer root", "", now.Add(-10*time.Minute)))
	persistTest(t, s, "a1", linkedTurn("recent continuation", "a", now))
	access := scalar(t, s, `SELECT accessed_ms FROM response_history_conversations WHERE id='a'`)
	expires := scalar(t, s, `SELECT expires_ms FROM response_history_conversations WHERE id='a'`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(path, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if scalar(t, s, `SELECT accessed_ms FROM response_history_conversations WHERE id='a'`) != access || scalar(t, s, `SELECT expires_ms FROM response_history_conversations WHERE id='a'`) != expires {
		t.Fatal("restart reset activity or idle expiry")
	}
	persistTest(t, s, "c", linkedTurn("new root", "", time.Now()))
	missing(t, s, "b")
	if _, ok := s.LoadResponse("a"); !ok {
		t.Fatal("restart lost LRU ordering")
	}
	// An idempotent direct retry must not revive an idle-expired root.
	raw := linkedTurn("old retry", "", time.Now())
	persistTest(t, s, "retry", raw)
	setIdle(t, s, "retry", now.Add(-2*time.Hour), time.Hour)
	if ok, err := s.PersistResponse("retry", raw, time.Hour); ok || !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired root revived: %v %v", ok, err)
	}
}
