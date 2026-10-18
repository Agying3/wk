package server

import (
	"container/heap"
	"encoding/json"
	"log"
	"time"

	"workbuddy2api/internal/responsehistory"
)

// Store immutable serialized turns so request normalization cannot mutate history.
type storedResponse struct {
	id          string
	raw         []byte
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

type responseHistoryPatch struct {
	TTLSeconds   *int64 `json:"ttl_seconds"`
	MaxEntries   *int   `json:"max_entries"`
	MaxStorageMB *int64 `json:"max_storage_mb"`
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
	cached := ok
	if !cached {
		if h.cfg.ResponseStore == nil {
			return storedResponseWire{}, false
		}
		raw, ok = h.cfg.ResponseStore.LoadResponse(id)
		if !ok {
			return storedResponseWire{}, false
		}
	}
	var wire storedResponseWire
	if json.Unmarshal(raw, &wire) != nil || len(wire.Messages) == 0 || wire.RouteKey == "" {
		return storedResponseWire{}, false
	}
	if !cached {
		created, expires := time.UnixMilli(wire.CreatedAt), time.UnixMilli(wire.ExpiresAt)
		if wire.CreatedAt == 0 || wire.ExpiresAt == 0 {
			return storedResponseWire{}, false
		}
		if !h.cacheResponse(id, raw, created, expires) {
			return storedResponseWire{}, false
		}
	}
	return wire, true
}

func (h *Handler) storeResponse(id string, messages []map[string]any, routeKey, parentID string) {
	now := time.Now()
	p := h.currentResponseHistoryPolicy()
	expires := now.Add(p.TTL())
	wire := storedResponseWire{Messages: messages, ParentID: parentID, RouteKey: routeKey,
		CreatedAt: now.UnixMilli(), ExpiresAt: expires.UnixMilli()}
	raw, err := json.Marshal(wire)
	if err != nil || !h.cacheResponse(id, raw, now, expires) {
		return
	}
	if h.cfg.ResponseStore != nil {
		h.cfg.ResponseStore.SaveResponse(id, raw, time.Until(expires))
	}
}

func (h *Handler) cacheResponse(id string, raw []byte, created, expires time.Time) bool {
	h.responsesMu.Lock()
	defer h.responsesMu.Unlock()
	now := time.Now()
	p := h.responsePolicy
	if deadline := created.Add(p.TTL()); deadline.Before(expires) {
		expires = deadline
	}
	// Reject a single oversized record before evicting useful existing entries.
	if !now.Before(expires) || int64(len(raw)) > p.MaxBytes() {
		return false
	}
	h.deleteResponseLocked(id)
	r := &storedResponse{id: id, raw: raw, createdAt: created, expiresAt: expires}
	h.responseHistory[id] = r
	h.responseBytes += int64(len(raw))
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
		h.responseBytes -= int64(len(r.raw))
		heap.Remove(&h.responseOrder, r.index)
		heap.Remove(&h.responseExpiry, r.expiryIndex)
		delete(h.responseHistory, id)
	}
}
