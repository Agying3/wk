package redisstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"workbuddy2api/internal/responsehistory"
)

// These integration tests require an explicitly isolated, disposable local Redis.
// The normal test suite never contacts a configured production Redis instance.
func newHistoryRedisTest(t *testing.T) *Upstash {
	t.Helper()
	addr := os.Getenv("WB2A_TEST_REDIS_ADDR")
	if addr == "" || os.Getenv("WB2A_TEST_REDIS_ISOLATED") != "1" {
		t.Skip("isolated Redis not supplied")
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatal("integration Redis must be loopback")
	}
	u := &Upstash{client: redis.NewClient(&redis.Options{Addr: addr})}
	ctx := context.Background()
	if err := u.client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	oldKeys := responseHistoryKeys
	prefix := fmt.Sprintf("wb2api:{history-test-%d}:", time.Now().UnixNano())
	responseHistoryKeys = []string{prefix + "data", prefix + "created", prefix + "expires", prefix + "sizes", prefix + "bytes", prefix + "policy"}
	t.Cleanup(func() {
		waitHistoryWrites(t, u)
		u.client.Del(ctx, responseHistoryKeys...)
		responseHistoryKeys = oldKeys
		u.client.Close()
	})
	return u
}

func waitHistoryWrites(t *testing.T, u *Upstash) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		u.versionMu.Lock()
		pending := len(u.versions)
		u.versionMu.Unlock()
		if pending == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("history writes did not drain")
}

func redisHistoryPayload(t *testing.T, body string, ttl time.Duration) []byte {
	t.Helper()
	now := time.Now()
	raw, err := json.Marshal(map[string]any{
		"messages":  []any{map[string]any{"role": "user", "content": body}},
		"route_key": "route", "created_at_unix_ms": now.UnixMilli(), "expires_at_unix_ms": now.Add(ttl).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestResponseHistoryLegacyReadWithoutConfigure(t *testing.T) {
	u := newHistoryRedisTest(t)
	ctx := context.Background()
	id := fmt.Sprintf("legacy-lazy-%d", time.Now().UnixNano())
	key := responsePrefix + id
	old := `{"messages":[{"role":"user","content":"literal image and text"}],"route_key":"route"}`
	if err := u.client.Set(ctx, key, old, 30*time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	defer u.client.Del(ctx, key)
	raw, ok := u.LoadResponse(id)
	if !ok {
		t.Fatal("legacy per-key history unavailable without configure")
	}
	var doc struct {
		Created int64  `json:"created_at_unix_ms"`
		Expires int64  `json:"expires_at_unix_ms"`
		Route   string `json:"route_key"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	remaining := time.Until(time.UnixMilli(doc.Expires))
	if doc.Route != "route" || doc.Expires-doc.Created != time.Hour.Milliseconds() || remaining > 30*time.Minute || remaining < 29*time.Minute {
		t.Fatalf("legacy timestamps: %+v", doc)
	}
	if u.client.Get(ctx, key).Val() != old {
		t.Fatal("read rewrote legacy Redis data")
	}
}

func TestResponseHistoryRedisSharedBudget(t *testing.T) {
	u := newHistoryRedisTest(t)
	p := responsehistory.Policy{TTLSeconds: 3600, MaxEntries: 2, MaxStorageMB: 1, MaxEntryMB: 32}
	if err := u.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		u.SaveResponse(id, redisHistoryPayload(t, strings.Repeat(id, 600000), time.Hour), time.Hour)
		waitHistoryWrites(t, u)
	}
	// Another client shares the exact same data budget, including after restart.
	other := &Upstash{client: redis.NewClient(u.client.Options())}
	defer other.client.Close()
	if _, ok := other.LoadResponse("a"); ok {
		t.Fatal("byte budget did not evict oldest record")
	}
	if _, ok := other.LoadResponse("b"); !ok {
		t.Fatal("latest persisted record missing")
	}
	used, err := other.client.Get(context.Background(), responseHistoryKeys[4]).Int64()
	if err != nil || used > 1000000 {
		t.Fatalf("shared bytes=%d err=%v", used, err)
	}
	u.SaveResponse("oversized", redisHistoryPayload(t, strings.Repeat("z", 1100000), time.Hour), time.Hour)
	waitHistoryWrites(t, u)
	if _, ok := other.LoadResponse("b"); !ok {
		t.Fatal("oversized insertion evicted valid record")
	}
	if _, ok := other.LoadResponse("oversized"); ok {
		t.Fatal("oversized record was persisted")
	}
	// Replace the large record with a small one and exercise the count limit.
	u.SaveResponse("b", redisHistoryPayload(t, "small", time.Hour), time.Hour)
	u.SaveResponse("c", redisHistoryPayload(t, "small", time.Hour), time.Hour)
	waitHistoryWrites(t, u)
	p.MaxEntries = 1
	if err := other.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	if n := u.client.HLen(context.Background(), responseHistoryKeys[0]).Val(); n != 1 {
		t.Fatalf("resize left %d records", n)
	}
}

func TestResponseHistoryRedisIndependentEntryLimit(t *testing.T) {
	u := newHistoryRedisTest(t)
	p := responsehistory.Default()
	p.MaxStorageMB, p.MaxEntryMB = 8, 2
	if err := u.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	for id, size := range map[string]int{"small-a": 300000, "large": 1500000, "small-b": 300000} {
		u.SaveResponse(id, redisHistoryPayload(t, strings.Repeat("x", size), time.Hour), time.Hour)
	}
	waitHistoryWrites(t, u)
	ctx := context.Background()
	before := u.client.Get(ctx, responseHistoryKeys[4]).Val()
	u.SaveResponse("oversized", redisHistoryPayload(t, strings.Repeat("x", 2100000), time.Hour), time.Hour)
	waitHistoryWrites(t, u)
	if u.client.HLen(ctx, responseHistoryKeys[0]).Val() != 3 || u.client.Get(ctx, responseHistoryKeys[4]).Val() != before {
		t.Fatal("per-entry rejection changed useful shared history")
	}
	other := &Upstash{client: redis.NewClient(u.client.Options())}
	defer other.client.Close()
	p.MaxEntryMB = 1
	if err := other.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := u.LoadResponse("large"); ok {
		t.Fatal("entry limit shrink left oversized shared history")
	}
	var used int64
	for _, id := range []string{"small-a", "small-b"} {
		if _, ok := u.LoadResponse(id); !ok {
			t.Fatal("entry limit shrink removed valid history")
		}
		used += u.client.HStrLen(ctx, responseHistoryKeys[0], id).Val()
	}
	if got := u.client.Get(ctx, responseHistoryKeys[4]).Val(); got != fmt.Sprint(used) {
		t.Fatalf("incorrect shared byte accounting after prune: %s want %d", got, used)
	}
	// A stale replica's writer must obey the latest shared per-entry policy.
	u.SaveResponse("stale-writer", redisHistoryPayload(t, strings.Repeat("x", 1100000), time.Hour), time.Hour)
	waitHistoryWrites(t, u)
	if _, ok := other.LoadResponse("stale-writer"); ok {
		t.Fatal("another replica bypassed the new entry limit")
	}
	p.MaxEntryMB, p.MaxStorageMB = 4, 1
	if err := other.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"new-a", "new-b"} {
		u.SaveResponse(id, redisHistoryPayload(t, strings.Repeat("x", 600000), time.Hour), time.Hour)
		waitHistoryWrites(t, u)
	}
	if _, ok := other.LoadResponse("new-a"); ok {
		t.Fatal("larger entry limit bypassed the total budget")
	}
	if _, ok := other.LoadResponse("new-b"); !ok {
		t.Fatal("latest record lost under independent total budget")
	}
}

func TestResponseHistoryRedisEntryLimitAppliesToMigration(t *testing.T) {
	u := newHistoryRedisTest(t)
	ctx := context.Background()
	legacyID := fmt.Sprintf("oversized-migration-%d", time.Now().UnixNano())
	legacyKey := responsePrefix + legacyID
	legacy, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("x", 1100000)}}, "route_key": "route"})
	if err := u.client.Set(ctx, legacyKey, legacy, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.client.Del(ctx, legacyKey) })
	p := responsehistory.Default()
	p.MaxEntryMB = 1
	if err := u.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := u.LoadResponse(legacyID); ok {
		t.Fatal("legacy migration bypassed single-entry limit")
	}
	if u.client.Exists(ctx, legacyKey).Val() != 0 {
		t.Fatal("oversized legacy key was left outside the configured budget")
	}
}

func TestResponseHistoryRedisTTLAndMigration(t *testing.T) {
	u := newHistoryRedisTest(t)
	ctx := context.Background()
	legacyID := fmt.Sprintf("history-migration-%d", time.Now().UnixNano())
	legacyKey := responsePrefix + legacyID
	legacy := `{"messages":[{"role":"user","content":"old"}],"route_key":"route"}`
	if err := u.client.Set(ctx, legacyKey, legacy, 30*time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.client.Del(ctx, legacyKey) })
	if err := u.ConfigureResponses(responsehistory.Default()); err != nil {
		t.Fatal(err)
	}
	raw, ok := u.LoadResponse(legacyID)
	if !ok || !strings.Contains(string(raw), "old") {
		t.Fatal("legacy history was not migrated")
	}
	if u.client.Exists(ctx, legacyKey).Val() != 0 {
		t.Fatal("legacy key was left outside the storage budget")
	}
	var doc struct {
		CreatedAt int64 `json:"created_at_unix_ms"`
		ExpiresAt int64 `json:"expires_at_unix_ms"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if time.Until(time.UnixMilli(doc.ExpiresAt)) > 30*time.Minute {
		t.Fatal("migration extended retention")
	}
	p := responsehistory.Default()
	p.TTLSeconds = 1
	if err := u.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := u.LoadResponse(legacyID); ok {
		t.Fatal("retention shrink did not prune existing record")
	}
	u.SaveResponse("short", redisHistoryPayload(t, "hello", time.Second), time.Second)
	waitHistoryWrites(t, u)
	first, ok := u.LoadResponse("short")
	if !ok {
		t.Fatal("short record missing immediately")
	}
	var firstDoc map[string]any
	json.Unmarshal(first, &firstDoc)
	second, _ := u.LoadResponse("short")
	var secondDoc map[string]any
	json.Unmarshal(second, &secondDoc)
	if firstDoc["expires_at_unix_ms"] != secondDoc["expires_at_unix_ms"] {
		t.Fatal("read renewed retention")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, ok := u.LoadResponse("short"); ok {
		t.Fatal("expired record restored")
	}
}

func TestResponseHistoryRedisBatchMigrationStaysWithinBudget(t *testing.T) {
	u := newHistoryRedisTest(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("batch-migration-%d-", time.Now().UnixNano())
	keys := make([]string, 120)
	pipe := u.client.Pipeline()
	for i := range keys {
		keys[i] = responsePrefix + fmt.Sprintf("%s%d", prefix, i)
		pipe.Set(ctx, keys[i], `{"messages":[{"role":"user","content":"legacy"}],"route_key":"route"}`, 30*time.Minute)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.client.Del(ctx, keys...) })
	p := responsehistory.Default()
	p.MaxEntries = 8
	if err := u.ConfigureResponses(p); err != nil {
		t.Fatal(err)
	}
	if n := u.client.Exists(ctx, keys...).Val(); n != 0 {
		t.Fatalf("%d legacy keys remain outside the budget", n)
	}
	if n := u.client.HLen(ctx, responseHistoryKeys[0]).Val(); n != 8 {
		t.Fatalf("batch retained %d entries, want 8", n)
	}
}
