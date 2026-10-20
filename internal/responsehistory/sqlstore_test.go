package responsehistory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func testTurn(text, image string, created time.Time) []byte {
	messages := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": text},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + image}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64," + image}},
	}}, map[string]any{"role": "assistant", "content": "你好\n", "tool_calls": []any{
		map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "test", "arguments": `{ "x": "\\\"\n" }`}},
	}}}
	raw, _ := json.Marshal(map[string]any{"messages": messages, "route_key": "route",
		"created_at_unix_ms": created.UnixMilli(), "expires_at_unix_ms": created.Add(time.Hour).UnixMilli()})
	return raw
}

func TestCodecExactRoundTrip(t *testing.T) {
	image := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0, 255, 31, 192, 90}, 20000))
	raw := testTurn(strings.Repeat("中文\n工具 JSON ", 3000), image, time.Now())
	packed, refs, images := splitImages(raw)
	if len(refs) != 2 || len(images) != 1 || len(packed) >= len(raw) {
		t.Fatal("image extraction/dedup failed")
	}
	for _, cold := range []bool{false, true} {
		payload, codec, err := encodePayload(packed, cold)
		if err != nil || codec != "zstd" || len(payload) >= len(packed)/2 {
			t.Fatalf("compression: %s %v", codec, err)
		}
		decoded, err := decodePayload(payload, codec, int64(len(packed)))
		if err != nil {
			t.Fatal(err)
		}
		restored, err := joinImages(decoded, refs, images, int64(len(raw)), digest(raw))
		if err != nil || !bytes.Equal(restored, raw) {
			t.Fatalf("byte mismatch: %v", err)
		}
	}
	for _, unusual := range []string{
		` { "url" : "data:image/png;base64,AB==" } `, // noncanonical padding bits
		`{"url":"data:image/png;base64,AA"}`,         // missing padding
		`{"url":"data:image/png;base64,AA==\n"}`,
		`{"url":"data:image/png;base64,\/w=="}`,
		`{"url":"https://example.invalid/image.png"}`,
	} {
		packed, refs, images := splitImages([]byte(unusual))
		restored, err := joinImages(packed, refs, images, int64(len(unusual)), digest([]byte(unusual)))
		if err != nil || string(restored) != unusual || len(images) != 0 {
			t.Fatalf("noncanonical changed: %v", err)
		}
	}
	images[refs[0].Hash][0] ^= 1
	if _, err := joinImages(packed, refs, images, int64(len(raw)), digest(raw)); err == nil {
		t.Fatal("corrupt image accepted")
	}
	if _, err := decodePayload([]byte("bad"), "zstd", 1024); err == nil {
		t.Fatal("corrupt zstd accepted")
	}
	if _, err := decodePayload([]byte("bad"), "other", 3); err == nil {
		t.Fatal("unknown codec accepted")
	}
}

func openTestSQLite(t *testing.T, p Policy) *SQLStore {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "responses.db"), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func persistTest(t *testing.T, s *SQLStore, id string, raw []byte) {
	t.Helper()
	ok, err := s.PersistResponse(id, raw, time.Hour)
	if err != nil || !ok {
		t.Fatalf("persist %s: %v %v", id, ok, err)
	}
}

func assertStored(t *testing.T, s *SQLStore, id string, want []byte) {
	t.Helper()
	got, ok := s.LoadResponse(id)
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("read %s mismatch (found=%v)", id, ok)
	}
}

func scalar(t *testing.T, s *SQLStore, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(s.bind(query), args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSQLiteRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses.db")
	p := Default()
	s, err := OpenSQLite(path, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := testTurn("durable", "AA==", time.Now())
	persistTest(t, s, "restart", raw)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(path, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertStored(t, s, "restart", raw)
}

func storeConformance(t *testing.T, s *SQLStore) {
	now := time.Now()
	image := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7, 255, 192, 0}, 16000))
	a := testTurn(strings.Repeat("重复文字与 JSON ", 3000), image, now)
	b := testTurn("newer", image, now.Add(time.Millisecond))
	persistTest(t, s, "a", a)
	persistTest(t, s, "b", b)
	assertStored(t, s, "a", a)
	if scalar(t, s, `SELECT COUNT(*) FROM response_history_images`) != 1 {
		t.Fatal("images not shared")
	}
	if scalar(t, s, `SELECT SUM(logical_bytes) FROM response_history_records`) != int64(len(a)+len(b)) {
		t.Fatal("logical accounting")
	}
	physical := scalar(t, s, `SELECT SUM(LENGTH(payload)) FROM response_history_records`) + scalar(t, s, `SELECT SUM(LENGTH(payload)) FROM response_history_images`)
	t.Logf("fixture before cold: logical=%d physical payload=%d ratio=%.2f", len(a)+len(b), physical, float64(len(a)+len(b))/float64(physical))
	for _, table := range []string{"response_history_records", "response_history_images"} {
		if _, err := s.db.Exec(s.bind(`UPDATE `+table+` SET accessed_ms=?`), now.Add(-20*time.Minute).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.maintain(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, `SELECT COUNT(*) FROM response_history_records WHERE tier=1`) != 2 || scalar(t, s, `SELECT COUNT(*) FROM response_history_images WHERE tier=1`) != 1 {
		t.Fatal("cold tier not applied")
	}
	assertStored(t, s, "a", a)
	assertStored(t, s, "b", b)
	// A recently accessed record must stay hot.
	c := testTurn("hot", image, now.Add(2*time.Millisecond))
	persistTest(t, s, "c", c)
	if err := s.maintain(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if scalar(t, s, `SELECT tier FROM response_history_records WHERE id='c'`) != 0 {
		t.Fatal("hot turn recompressed")
	}
	p := Default()
	p.MaxEntries = 2
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LoadResponse("a"); ok {
		t.Fatal("oldest survived")
	}
	if scalar(t, s, `SELECT COUNT(*) FROM response_history_images`) != 1 {
		t.Fatal("shared image prematurely deleted")
	}
	// Compression and dedup do not bypass logical limits.
	p.MaxStorageMB = 1
	p.MaxEntryMB = 1
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	oversized := testTurn(strings.Repeat("x", 1100000), image, now)
	if ok, err := s.PersistResponse("huge", oversized, time.Hour); !errors.Is(err, ErrCapacity) || ok {
		t.Fatalf("oversized: %v %v", ok, err)
	}
	assertStored(t, s, "b", b)
	assertStored(t, s, "c", c)
	// Independent total budget evicts by uncompressed size even for tiny zstd.
	persistTest(t, s, "large1", testTurn(strings.Repeat("x", 600000), "", now.Add(3*time.Millisecond)))
	persistTest(t, s, "large2", testTurn(strings.Repeat("y", 600000), "", now.Add(4*time.Millisecond)))
	if _, ok := s.LoadResponse("large1"); ok {
		t.Fatal("compressed bytes bypassed budget")
	}
	if scalar(t, s, `SELECT COUNT(*) FROM response_history_images`) != 0 {
		t.Fatal("orphan images leaked")
	}
	if scalar(t, s, `SELECT COUNT(*) FROM response_history_image_refs`) != 0 {
		t.Fatal("orphan refs leaked")
	}
	p.MaxEntryMB = 2
	p.MaxStorageMB = 3
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	persistTest(t, s, "single", testTurn(strings.Repeat("s", 1100000), "", now))
	p.MaxEntryMB = 1
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LoadResponse("single"); ok {
		t.Fatal("entry resize failed")
	}
	// Shortened TTL uses original creation time and does not revive on extension.
	p.TTLSeconds = 1
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	old := testTurn("expired", image, now.Add(-2*time.Second))
	if ok, err := s.PersistResponse("old", old, time.Hour); !errors.Is(err, ErrNotFound) || ok {
		t.Fatalf("expired accepted: %v", err)
	}
	p.TTLSeconds = 3600
	if err := s.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LoadResponse("old"); ok {
		t.Fatal("TTL renewed")
	}
}

func TestSQLiteConformance(t *testing.T) { storeConformance(t, openTestSQLite(t, Default())) }

func TestSQLiteCorruption(t *testing.T) {
	s := openTestSQLite(t, Default())
	raw := testTurn(strings.Repeat("text", 5000), "AA==", time.Now())
	persistTest(t, s, "corrupt", raw)
	if _, err := s.db.Exec(`UPDATE response_history_images SET payload=?`, []byte{99}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LoadResponse("corrupt"); ok {
		t.Fatal("corrupt image served")
	}
	if _, err := s.db.Exec(`UPDATE response_history_records SET payload=?`, []byte("broken")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LoadResponse("corrupt"); ok {
		t.Fatal("corrupt document served")
	}
}

type legacyFixture map[string][]byte

func (l legacyFixture) SaveResponse(string, []byte, time.Duration) {}
func (l legacyFixture) LoadResponse(id string) ([]byte, bool)      { raw, ok := l[id]; return raw, ok }

func TestSQLiteLegacyMigrationDoesNotReviveEvicted(t *testing.T) {
	p := Default()
	p.MaxEntries = 1
	now := time.Now()
	old := testTurn("old Redis", "AA==", now.Add(-time.Minute))
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "responses.db"), p, legacyFixture{"old": old})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertStored(t, s, "old", old)
	persistTest(t, s, "new", testTurn("new", "", now))
	if _, ok := s.LoadResponse("old"); ok {
		t.Fatal("evicted Redis history revived")
	}
}

func concurrentBudget(t *testing.T, stores ...*SQLStore) {
	p := Default()
	p.MaxEntries = 3
	p.MaxStorageMB = 1
	p.MaxEntryMB = 1
	if err := stores[0].ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				s := stores[worker%len(stores)]
				id := fmt.Sprintf("worker-%d-%d", worker, i)
				raw := testTurn(strings.Repeat("x", 240000), "AA==", time.Now())
				if _, err := s.PersistResponse(id, raw, time.Hour); err != nil {
					t.Errorf("concurrent save: %v", err)
					return
				}
				s.LoadResponse(id) // Another writer may legitimately evict this turn.
			}
		}(worker)
	}
	wg.Wait()
	if scalar(t, stores[0], `SELECT COUNT(*) FROM response_history_records`) > 3 || scalar(t, stores[0], `SELECT COALESCE(SUM(logical_bytes),0) FROM response_history_records`) > 1000000 {
		t.Fatal("shared budget exceeded")
	}
}

func TestSQLiteConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses.db")
	s1, err := OpenSQLite(path, Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := OpenSQLite(path, Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	concurrentBudget(t, s1, s2)
}

func TestPostgresConformance(t *testing.T) {
	dsn := os.Getenv("WB2A_HISTORY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set WB2A_HISTORY_TEST_POSTGRES_DSN to an isolated loopback database")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Path != "/wb2a_history_test" {
		t.Fatal("test DSN must target loopback/wb2a_history_test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Tests use only their own freshly created schema in the isolated test DB.
	schema := fmt.Sprintf("history_test_%d", time.Now().UnixNano())
	if _, err := db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	isolated, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer isolated.Close()
	s1, err := OpenSQL(isolated, true, Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	storeConformance(t, s1)
	s2, err := OpenSQL(isolated, true, Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	resetHistoryTestStore(t, s1)
	conversationConformance(t, s1)
	resetHistoryTestStore(t, s1)
	leaseConformance(t, s1, s2)
	resetHistoryTestStore(t, s1)
	concurrentConversationForks(t, s1, s2)
	resetHistoryTestStore(t, s1)
	concurrentBudget(t, s1, s2)
}

func resetHistoryTestStore(t *testing.T, s *SQLStore) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, table := range []string{"response_history_records", "response_history_conversations", "response_history_leases"} {
		if _, err = tx.Exec(`DELETE FROM ` + table); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.prune(ctx, tx, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
