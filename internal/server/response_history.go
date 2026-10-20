package server

import (
	"container/heap"
	"encoding/json"
	"errors"
	"log"
	"time"

	"workbuddy2api/internal/responsehistory"
)

// Store immutable serialized turns so request normalization cannot mutate history.
type storedResponse struct {
	id          string
	raw         []byte
	size        int64
	createdAt   time.Time
	expiresAt   time.Time
	index       int
	expiryIndex int
}

type responseHistoryHeap []*storedResponse

type responseExpiryHeap []*storedResponse

func (h responseExpiryHeap) Len() int { return len(h) }
func (h responseExpiryHeap) Less(i, j int) bool {
	if h[i].expiresAt.Equal(h[j].expiresAt) {
		return h[i].id < h[j].id
	}
	return h[i].expiresAt.Before(h[j].expiresAt)
}
func (h responseExpiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].expiryIndex, h[j].expiryIndex = i, j
}
func (h *responseExpiryHeap) Push(x any) {
	r := x.(*storedResponse)
	r.expiryIndex = len(*h)
	*h = append(*h, r)
}
func (h *responseExpiryHeap) Pop() any {
	old := *h
	r := old[len(old)-1]
	old[len(old)-1] = nil
	r.expiryIndex = -1
	*h = old[:len(old)-1]
	return r
}

func (h responseHistoryHeap) Len() int { return len(h) }
func (h responseHistoryHeap) Less(i, j int) bool {
	if h[i].createdAt.Equal(h[j].createdAt) {
		return h[i].id < h[j].id
	}
	return h[i].createdAt.Before(h[j].createdAt)
}
func (h responseHistoryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *responseHistoryHeap) Push(x any) {
	r := x.(*storedResponse)
	r.index = len(*h)
	*h = append(*h, r)
}
func (h *responseHistoryHeap) Pop() any {
	old := *h
	r := old[len(old)-1]
	old[len(old)-1] = nil
	r.index = -1
	*h = old[:len(old)-1]
	return r
}

type responsePolicyStore interface {
	ConfigureResponses(responsehistory.Policy) error
}

type durableResponseStore interface {
	ResponseHistoryOnDisk() bool
	PersistResponse(string, []byte, time.Duration) (bool, error)
}

func (h *Handler) responseHistoryOnDisk() bool {
	s, ok := h.cfg.ResponseStore.(durableResponseStore)
	return ok && s.ResponseHistoryOnDisk()
}

func (h *Handler) responseHistoryStorage() string {
	if s, ok := h.cfg.ResponseStore.(interface{ ResponseHistoryStorage() string }); ok {
		return s.ResponseHistoryStorage()
	}
	if _, ok := h.cfg.ResponseStore.(responsePolicyStore); ok {
		return "redis"
	}
	return "memory"
}

type responseHistoryPatch struct {
	TTLSeconds   *int64 `json:"ttl_seconds"`
	MaxEntries   *int   `json:"max_entries"`
	MaxStorageMB *int64 `json:"max_storage_mb"`
	MaxEntryMB   *int64 `json:"max_entry_mb"`
}

func (p responseHistoryPatch) apply(current responsehistory.Policy) responsehistory.Policy {
	if p.TTLSeconds != nil {
		current.TTLSeconds = *p.TTLSeconds
	}
	if p.MaxEntries != nil {
		current.MaxEntries = *p.MaxEntries
	}
	if p.MaxStorageMB != nil {
		current.MaxStorageMB = *p.MaxStorageMB
	}
	if p.MaxEntryMB != nil {
		current.MaxEntryMB = *p.MaxEntryMB
	}
	return current
}

func (h *Handler) currentResponseHistoryPolicy() responsehistory.Policy {
	h.responsesMu.Lock()
	defer h.responsesMu.Unlock()
	return h.responsePolicy
}

// Returns whether the optional remote store also applied the policy. Local limits
// always apply immediately; a failed remote update is reported to the admin UI.
func (h *Handler) setResponseHistoryPolicy(p responsehistory.Policy) bool {
	h.responsesMu.Lock()
	h.responsePolicy = p
	now := time.Now()
	for _, r := range h.responseHistory {
		// Shortening retention trims existing entries; extending never revives
		// expired data or extends an entry beyond its original deadline.
		if deadline := r.createdAt.Add(p.TTL()); deadline.Before(r.expiresAt) {
			r.expiresAt = deadline
		}
	}
	heap.Init(&h.responseExpiry)
	for id, r := range h.responseHistory {
		if r.size > p.MaxEntryBytes() {
			h.deleteResponseLocked(id)
		}
	}
	h.trimResponsesLocked(now)
	h.responsesMu.Unlock()
	if store, ok := h.cfg.ResponseStore.(responsePolicyStore); ok {
		if err := store.ConfigureResponses(p); err != nil {
			log.Printf("[responses] remote history policy update failed: %v", err)
			return false
		}
	}
	return true
}

func (h *Handler) loadResponseRecord(id string) (storedResponseWire, bool) {
	wire, ok, _ := h.loadResponseRecordResult(id)
	return wire, ok
}

func (h *Handler) loadResponseRecordResult(id string) (storedResponseWire, bool, error) {
	now := time.Now()
	h.responsesMu.Lock()
	h.trimResponsesLocked(now)
	r, ok := h.responseHistory[id]
	if ok && !now.Before(r.expiresAt) {
		h.deleteResponseLocked(id)
		ok = false
	}
	var raw []byte
	if ok {
		raw = r.raw
	}
	h.responsesMu.Unlock()
	// A disk-backed entry contains only metadata. Always read the authoritative
	// database, including when another replica has evicted or shortened this turn.
	cached := ok && !h.responseHistoryOnDisk()
	if !cached {
		if h.cfg.ResponseStore == nil {
			return storedResponseWire{}, false, nil
		}
		if reader, typed := h.cfg.ResponseStore.(interface{ ReadResponse(string) ([]byte, error) }); typed {
			var err error
			raw, err = reader.ReadResponse(id)
			if err != nil {
				return storedResponseWire{}, false, err
			}
			ok = true
		} else {
			raw, ok = h.cfg.ResponseStore.LoadResponse(id)
		}
		if !ok {
			h.responsesMu.Lock()
			h.deleteResponseLocked(id)
			h.responsesMu.Unlock()
			return storedResponseWire{}, false, nil
		}
	}
	var wire storedResponseWire
	if json.Unmarshal(raw, &wire) != nil || len(wire.Messages) == 0 || wire.RouteKey == "" {
		return storedResponseWire{}, false, nil
	}
	if !cached {
		created, expires := time.UnixMilli(wire.CreatedAt), time.UnixMilli(wire.ExpiresAt)
		if wire.CreatedAt == 0 || wire.ExpiresAt == 0 {
			return storedResponseWire{}, false, nil
		}
		if !h.cacheResponse(id, raw, created, expires) && !h.responseHistoryOnDisk() {
			return storedResponseWire{}, false, nil
		}
	}
	return wire, true, nil
}

func (h *Handler) storeResponse(id string, messages []map[string]any, routeKey, parentID string) error {
	now := time.Now()
	p := h.currentResponseHistoryPolicy()
	expires := now.Add(p.TTL())
	wire := storedResponseWire{Messages: messages, ParentID: parentID, RouteKey: routeKey,
		CreatedAt: now.UnixMilli(), ExpiresAt: expires.UnixMilli()}
	raw, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	if store, ok := h.cfg.ResponseStore.(durableResponseStore); ok && store.ResponseHistoryOnDisk() {
		saved, err := store.PersistResponse(id, raw, time.Until(expires))
		if err != nil {
			log.Printf("[responses] durable history write failed: %v", err)
		}
		if saved {
			h.cacheResponse(id, raw, now, expires)
		}
		if err != nil {
			return err
		}
		if !saved {
			return responsehistory.ErrCapacity
		}
		return nil
	}
	if !h.cacheResponse(id, raw, now, expires) {
		return responsehistory.ErrCapacity
	}
	if h.cfg.ResponseStore != nil {
		h.cfg.ResponseStore.SaveResponse(id, raw, time.Until(expires))
	}
	return nil
}

func responseHistoryResult(response map[string]any, err error) {
	if err == nil {
		return
	}
	code := "response_history_unavailable"
	message := "Response generated, but its history could not be persisted; retry with the complete conversation."
	if errors.Is(err, responsehistory.ErrCapacity) {
		code = "response_history_capacity_exceeded"
		message = "Response generated, but this conversation cannot fit the configured history limits; retry with the complete conversation."
	}
	if errors.Is(err, responsehistory.ErrNotFound) {
		code = "previous_response_not_found"
	}
	response["history_stored"] = false
	response["history_error"] = map[string]any{"code": code, "message": message}
}

func (h *Handler) cacheResponse(id string, raw []byte, created, expires time.Time) bool {
	h.responsesMu.Lock()
	defer h.responsesMu.Unlock()
	now := time.Now()
	p := h.responsePolicy
	if h.responseHistoryOnDisk() {
		// Original JSON timestamps are immutable. SQL conversation metadata owns
		// the renewable idle deadline; this cache is only a disposable index.
		expires = now.Add(p.TTL())
	} else if deadline := created.Add(p.TTL()); deadline.Before(expires) {
		expires = deadline
	}
	// Reject a single oversized record before evicting useful existing entries.
	if !now.Before(expires) || int64(len(raw)) > p.MaxBytes() || int64(len(raw)) > p.MaxEntryBytes() {
		return false
	}
	h.deleteResponseLocked(id)
	r := &storedResponse{id: id, raw: raw, size: int64(len(raw)), createdAt: created, expiresAt: expires}
	if h.responseHistoryOnDisk() {
		r.raw = nil
	}
	h.responseHistory[id] = r
	h.responseBytes += r.size
	heap.Push(&h.responseOrder, r)
	heap.Push(&h.responseExpiry, r)
	h.trimResponsesLocked(now)
	return h.responseHistory[id] == r
}

func (h *Handler) trimResponsesLocked(now time.Time) {
	for len(h.responseExpiry) > 0 && !now.Before(h.responseExpiry[0].expiresAt) {
		h.deleteResponseLocked(h.responseExpiry[0].id)
	}
	// Creation order permits O(log N) oldest-first eviction at large entry counts.
	// A separate expiration index removes expired entries before live eviction.
	for len(h.responseOrder) > 0 {
		r := h.responseOrder[0]
		if now.Before(r.expiresAt) && len(h.responseHistory) <= h.responsePolicy.MaxEntries && h.responseBytes <= h.responsePolicy.MaxBytes() {
			break
		}
		h.deleteResponseLocked(r.id)
	}
}

func (h *Handler) deleteResponseLocked(id string) {
	if r, ok := h.responseHistory[id]; ok {
		h.responseBytes -= r.size
		heap.Remove(&h.responseOrder, r.index)
		heap.Remove(&h.responseExpiry, r.expiryIndex)
		delete(h.responseHistory, id)
	}
}
