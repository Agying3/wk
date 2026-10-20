package redisstore

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"workbuddy2api/internal/responsehistory"
)

// One shared budget across replicas, independent of pool/session keys. A hash
// stores payloads; sorted indexes track creation and expiration without SCAN on
// the request path. The hash tag also keeps script keys in one Redis Cluster slot.
var responseHistoryKeys = []string{
	"wb2api:{response-history}:data", "wb2api:{response-history}:created",
	"wb2api:{response-history}:expires", "wb2api:{response-history}:sizes",
	"wb2api:{response-history}:bytes", "wb2api:{response-history}:policy",
}

var responseHistoryScript = redis.NewScript(`
local mode, id, data = ARGV[1], ARGV[2], ARGV[3]
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local used = tonumber(redis.call('GET', KEYS[5]) or '0')
local function remove(member)
  used = math.max(0, used - tonumber(redis.call('HGET', KEYS[4], member) or '0'))
  redis.call('HDEL', KEYS[1], member)
  redis.call('ZREM', KEYS[2], member)
  redis.call('ZREM', KEYS[3], member)
  redis.call('HDEL', KEYS[4], member)
end
if mode == 'configure' then
  redis.call('HSET', KEYS[6], 'ttl', ARGV[4], 'entries', ARGV[5], 'bytes', ARGV[6], 'entry_bytes', ARGV[9] or '32000000')
end
local ttl = tonumber(redis.call('HGET', KEYS[6], 'ttl') or '3600000')
local maxEntries = tonumber(redis.call('HGET', KEYS[6], 'entries') or '1024')
local maxBytes = tonumber(redis.call('HGET', KEYS[6], 'bytes') or '64000000')
local maxEntryBytes = tonumber(redis.call('HGET', KEYS[6], 'entry_bytes') or '32000000')
if mode == 'configure' then
  local entries = redis.call('ZRANGE', KEYS[2], 0, -1, 'WITHSCORES')
  for i = 1, #entries, 2 do
    local size = tonumber(redis.call('HGET', KEYS[4], entries[i]) or '0')
    if size > maxEntryBytes then
      remove(entries[i])
    else
      local expiry = tonumber(redis.call('ZSCORE', KEYS[3], entries[i]) or '0')
      redis.call('ZADD', KEYS[3], math.min(expiry, tonumber(entries[i+1]) + ttl), entries[i])
    end
  end
end
for _, member in ipairs(redis.call('ZRANGEBYSCORE', KEYS[3], '-inf', now)) do remove(member) end
if mode == 'save' or mode == 'migrate' then
  -- Oversized records must not evict unrelated useful history.
  if #data <= maxBytes and #data <= maxEntryBytes then
    remove(id)
    local birth = tonumber(ARGV[8])
    local expiry = math.min(now + tonumber(ARGV[7]), birth + ttl)
    if expiry > now then
      redis.call('HSET', KEYS[1], id, data)
      redis.call('ZADD', KEYS[2], birth, id)
      redis.call('ZADD', KEYS[3], expiry, id)
      redis.call('HSET', KEYS[4], id, #data)
      used = used + #data
    end
  end
end
while redis.call('HLEN', KEYS[1]) > maxEntries or used > maxBytes do
  local oldest = redis.call('ZRANGE', KEYS[2], 0, 0)
  if #oldest == 0 then break end
  remove(oldest[1])
end
redis.call('SET', KEYS[5], used)
-- Expire payloads and indexes even if the service stops making requests.
local latest = redis.call('ZREVRANGE', KEYS[3], 0, 0, 'WITHSCORES')
if #latest == 0 then
  for i = 1, 5 do redis.call('DEL', KEYS[i]) end
else
  for i = 1, 5 do redis.call('PEXPIREAT', KEYS[i], latest[2]) end
end
if mode == 'load' then
  local value = redis.call('HGET', KEYS[1], id)
  if not value then return false end
  -- Preserve the actual remaining deadline on cache refill (including a
  -- retention reduction applied by another replica).
  return {value, tonumber(redis.call('ZSCORE', KEYS[3], id))}
end
return 1
`)

func (u *Upstash) ConfigureResponses(p responsehistory.Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	u.responseMu.Lock()
	defer u.responseMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := responseHistoryScript.Run(ctx, u.client, responseHistoryKeys,
		"configure", "", "", p.TTL().Milliseconds(), p.MaxEntries, p.MaxBytes(), 0, 0, p.MaxEntryBytes()).Err(); err != nil {
		return err
	}
	// Import old per-key history once during upgrade, preserving remaining TTL.
	// Each import enforces the same global budget before removing the old key.
	iter := u.client.Scan(ctx, 0, responsePrefix+"*", 100).Iterator()
	batch := make([]string, 0, 100)
	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) == cap(batch) {
			if err := u.migrateResponseBatch(ctx, batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if err := iter.Err(); err != nil {
		return err
	}
	return u.migrateResponseBatch(ctx, batch)
}

// Batch network round trips: a 1,024-turn legacy history must not require over
// 4,000 sequential requests to a remote Upstash instance during startup.
func (u *Upstash) migrateResponseBatch(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	reads := u.client.Pipeline()
	readStarted := time.Now()
	values := make([]*redis.StringCmd, len(keys))
	ttls := make([]*redis.DurationCmd, len(keys))
	for i, key := range keys {
		values[i] = reads.Get(ctx, key)
		ttls[i] = reads.PTTL(ctx, key)
	}
	if _, err := reads.Exec(ctx); err != nil && err != redis.Nil {
		return err
	}
	writes := u.client.Pipeline()
	migrated := make([]string, 0, len(keys))
	for i, key := range keys {
		raw, err := values[i].Bytes()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			return err
		}
		originalRemaining, err := ttls[i].Result()
		if err != nil {
			return err
		}
		deadline := readStarted.Add(originalRemaining)
		remaining := time.Until(deadline)
		if remaining <= 0 {
			continue
		}
		birth := deadline.Add(-time.Hour)
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil || doc == nil {
			continue
		}
		doc["created_at_unix_ms"], doc["expires_at_unix_ms"] = birth.UnixMilli(), deadline.UnixMilli()
		raw, err = json.Marshal(doc)
		if err != nil {
			return err
		}
		// The configure call has loaded this script before a migration starts.
		responseHistoryScript.Run(ctx, writes, responseHistoryKeys,
			"migrate", key[len(responsePrefix):], raw, 0, 0, 0, remaining.Milliseconds(), birth.UnixMilli())
		migrated = append(migrated, key)
	}
	if len(migrated) == 0 {
		return nil
	}
	if _, err := writes.Exec(ctx); err != nil {
		return err
	}
	// Never delete legacy data unless all import commands succeeded.
	return u.client.Del(ctx, migrated...).Err()
}

func (u *Upstash) SaveResponse(id string, data []byte, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	deadline := time.Now().Add(ttl)
	var meta struct {
		CreatedAt int64 `json:"created_at_unix_ms"`
		ExpiresAt int64 `json:"expires_at_unix_ms"`
	}
	if json.Unmarshal(data, &meta) != nil || meta.CreatedAt == 0 {
		return
	}
	if expiry := time.UnixMilli(meta.ExpiresAt); meta.ExpiresAt > 0 && expiry.Before(deadline) {
		deadline = expiry
	}
	snapshot := append([]byte(nil), data...)
	u.scheduleWrite(responsePrefix+id, "SaveResponse "+id, func(ctx context.Context) error {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		return responseHistoryScript.Run(ctx, u.client, responseHistoryKeys,
			"save", id, snapshot, 0, 0, 0, remaining.Milliseconds(), meta.CreatedAt).Err()
	})
}

func (u *Upstash) LoadResponse(id string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	values, err := responseHistoryScript.Run(ctx, u.client, responseHistoryKeys,
		"load", id, "", 0, 0, 0, 0).Slice()
	if err != nil && err != redis.Nil {
		log.Printf("[redisstore] response history read failed: %v", err)
	}
	if err != nil && err != redis.Nil {
		return nil, false
	}
	if len(values) != 2 {
		return u.loadLegacyResponse(ctx, id)
	}
	raw, valid := values[0].(string)
	expires, validExpiry := values[1].(int64)
	if !valid || !validExpiry {
		return nil, false
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &doc) != nil || doc == nil {
		return nil, false
	}
	doc["expires_at_unix_ms"] = json.RawMessage(strconv.FormatInt(expires, 10))
	payload, err := json.Marshal(doc)
	return payload, err == nil
}

// Read the pre-budget per-key format lazily without a SCAN or a Redis rewrite.
// Those records used a fixed one-hour TTL and lacked timestamp fields.
func (u *Upstash) loadLegacyResponse(ctx context.Context, id string) ([]byte, bool) {
	started := time.Now()
	pipe := u.client.Pipeline()
	value := pipe.Get(ctx, responsePrefix+id)
	ttl := pipe.PTTL(ctx, responsePrefix+id)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, false
	}
	remaining, err := ttl.Result()
	if err != nil || remaining <= 0 {
		return nil, false
	}
	deadline := started.Add(remaining)
	var doc map[string]json.RawMessage
	if json.Unmarshal([]byte(value.Val()), &doc) != nil || doc == nil {
		return nil, false
	}
	doc["created_at_unix_ms"] = json.RawMessage(strconv.FormatInt(deadline.Add(-time.Hour).UnixMilli(), 10))
	doc["expires_at_unix_ms"] = json.RawMessage(strconv.FormatInt(deadline.UnixMilli(), 10))
	raw, err := json.Marshal(doc)
	return raw, err == nil
}
