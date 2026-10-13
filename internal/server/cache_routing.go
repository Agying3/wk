package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/cacheroute"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
)

type chatRoute struct {
	h                  *Handler
	key, source, model string
	lease              session.Lease
	trace              cacheroute.Trace
	wait               time.Duration
	predicted          int
	reason, shadowUID  string
	queueReason        string
	cacheKnown         bool
	tenant             string
}

func (c *chatRoute) admissionEnabled() bool {
	return c.h.cacheRouter != nil && !c.h.cacheRouter.Config().Shadow
}
func (c *chatRoute) request(related bool) pool.RouteRequest {
	return pool.RouteRequest{Tenant: c.tenant, Session: c.key, Prefix: c.trace.Root, Related: related}
}
func (c *chatRoute) pickUID(uid string, related bool, tried, allow map[string]bool) *auth.Auth {
	if c.admissionEnabled() {
		return c.h.cfg.Pool.PickAndAcquireByUIDForRoute(uid, c.model, c.request(related), tried, allow)
	}
	return c.h.cfg.Pool.PickAndAcquireByUIDForModel(uid, c.model)
}
func (c *chatRoute) release(uid string) {
	if c.admissionEnabled() {
		c.h.cfg.Pool.ReleaseRoute(uid, c.request(false))
	} else {
		c.h.cfg.Pool.Release(uid)
	}
}

func routingIdentity(doc map[string]any, r *http.Request, allowUserID bool) (string, string) {
	if meta, ok := doc["metadata"].(map[string]any); ok {
		if v, ok := meta["conversation_id"].(string); ok && v != "" {
			return v, "metadata.conversation_id"
		}
	}
	for _, key := range []string{"conversation_id", "conversation", "prompt_cache_key"} {
		if v, ok := doc[key].(string); ok && v != "" {
			return v, key
		}
	}
	for _, key := range []string{"X-WorkBuddy-Session-ID", "X-Session-ID", "X-Conversation-ID"} {
		if v := r.Header.Get(key); v != "" {
			return v, key
		}
	}
	if allowUserID {
		if meta, ok := doc["metadata"].(map[string]any); ok {
			if v, ok := meta["user_id"].(string); ok && v != "" {
				return v, "metadata.user_id"
			}
		}
	}
	return "", "none"
}

func routeNamespace(r *http.Request) string {
	s := scopeFromRequest(r)
	return fmt.Sprintf("%q/%q/%q", s.kind, s.group, s.keyID)
}

func (h *Handler) beginChatRoute(r *http.Request, raw, source, model string, doc map[string]any, allow map[string]bool) *chatRoute {
	c := &chatRoute{h: h, source: source, model: model, reason: "rotation"}
	preferred := ""
	states := h.cfg.Pool.RoutingStates(model, allow)
	if h.cacheRouter != nil {
		c.tenant = h.cacheRouter.Hash(routeNamespace(r))
		c.trace = h.cacheRouter.Build(routeNamespace(r), model, doc)
		if raw == "" && c.trace.Root != "" && !h.cacheRouter.Config().Shadow {
			raw = "prefix:" + c.trace.Root
			c.source = "prefix"
		}
		if plans := h.cacheRouter.Plans("", c.trace, states, nil); len(plans) > 0 && !h.cacheRouter.Config().Shadow {
			preferred = plans[0].UID
		}
	}
	if raw == "" || h.cfg.Session == nil {
		return c
	}
	c.key = cacheroute.ScopedKey(routeNamespace(r), model, raw)
	if _, exists := h.cfg.Session.LookupLease(c.key); !exists && c.source != "prefix" {
		if legacy, ok := h.cfg.Session.LookupLease(raw); ok {
			for _, state := range states {
				if state.UID == legacy.UID && state.Healthy {
					preferred = legacy.UID
					break
				}
			}
		}
	}
	if preferred == "" && c.admissionEnabled() && !h.cacheRouter.Config().ProtectionDisabled {
		preferred = h.cfg.Pool.RecommendRoute(model, c.request(false), nil, allow)
	}
	var healthy, available []string
	for _, s := range states {
		if s.Healthy {
			healthy = append(healthy, s.UID)
			if s.Waiting == 0 && (s.Limit <= 0 || s.InFlight < s.Limit) {
				available = append(available, s.UID)
			}
		}
	}
	sort.Strings(healthy)
	sort.Strings(available)
	c.lease, _ = h.cfg.Session.ResolveLeaseWithCandidates(c.key, allow, preferred, healthy, available)
	return c
}

func (c *chatRoute) acquire(ctx context.Context, tried, allow map[string]bool) (*auth.Auth, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := c.h.cfg.Pool
	states := p.RoutingStates(c.model, allow)
	var acct *auth.Auth
	c.predicted = 0
	if c.h.cacheRouter != nil {
		cfg := c.h.cacheRouter.Config()
		plans := c.h.cacheRouter.Plans(c.lease.UID, c.trace, states, tried)
		if len(plans) > 0 {
			plan := plans[0]
			if cfg.Shadow {
				c.shadowUID = plan.UID
			} else {
				remaining := cfg.WaitTimeout - c.wait
				// A comparably warm ready candidate can bypass a predicted
				// wait that exceeds the remaining queue budget.
				if plan.QueueSeconds > remaining.Seconds() {
					for _, other := range plans {
						if other.QueueSeconds == 0 {
							plan = other
							break
						}
					}
				}
				acct = c.pickUID(plan.UID, plan.CacheTokens >= 512, tried, allow)
				if acct == nil && remaining > 0 {
					start := time.Now()
					waitCtx, cancel := context.WithTimeout(ctx, remaining)
					var waitErr error
					acct, waitErr = p.WaitAndAcquireByUIDForRoute(waitCtx, plan.UID, c.model, cfg.MaxPending, cfg.MaxPendingTotal, c.request(plan.CacheTokens >= 512), tried, allow)
					if waitErr != nil {
						c.queueReason = waitErr.Error()
					} else {
						c.queueReason = "slot_released"
					}
					cancel()
					c.wait += time.Since(start)
					if err := ctx.Err(); err != nil {
						if acct != nil {
							c.release(acct.UID)
						}
						return nil, err
					}
				}
				if acct != nil {
					c.predicted = plan.CacheTokens
					c.reason = "prefix"
					if acct.UID == c.lease.UID {
						c.reason = "primary"
					}
				} else {
					// Budget exhausted/queue full: reconsider warm accounts
					// before permitting a cold availability fallback.
					for _, other := range c.h.cacheRouter.Plans(c.lease.UID, c.trace, p.RoutingStates(c.model, allow), tried) {
						if acct = c.pickUID(other.UID, other.CacheTokens >= 512, tried, allow); acct != nil {
							c.predicted = other.CacheTokens
							c.reason = "warm_borrow"
							break
						}
					}
				}
			}
		}
	}
	if acct == nil && c.lease.UID != "" && !tried[c.lease.UID] && (allow == nil || allow[c.lease.UID]) {
		acct = c.pickUID(c.lease.UID, false, tried, allow)
		if acct != nil {
			c.reason = "primary"
		}
	}
	if acct == nil {
		if c.admissionEnabled() && !c.h.cacheRouter.Config().ProtectionDisabled {
			acct = p.PickAndAcquireRoute(c.model, c.request(false), tried, allow)
		} else {
			acct = p.PickAndAcquireForModelAllow(c.model, tried, allow)
		}
		if acct != nil {
			c.reason = "availability_fallback"
		}
	}
	if acct == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		c.release(acct.UID)
		return nil, err
	}
	if c.key != "" && c.lease.UID != "" && acct.UID != c.lease.UID {
		healthy := p.HealthyForModel(c.lease.UID, c.model) && (allow == nil || allow[c.lease.UID])
		if !healthy {
			if lease, ok := c.h.cfg.Session.ReplaceIfCurrent(c.key, c.lease, acct.UID); ok {
				c.lease = lease
				c.reason = "primary_unavailable"
			}
		} else if c.reason == "prefix" {
			c.reason = "warm_borrow"
		}
	}
	return acct, nil
}

func (c *chatRoute) success(st *chatStat, sent time.Time, cacheKnown bool) {
	c.cacheKnown = cacheKnown
	if c.admissionEnabled() {
		c.h.cfg.Pool.NoteRouteSuccess(st.uid, c.request(false), time.Since(sent))
	}
	if c.h.cfg.Session != nil && c.key != "" {
		if c.lease.UID == "" {
			c.lease, _ = c.h.cfg.Session.BindIfAbsent(c.key, st.uid)
		} else {
			c.h.cfg.Session.TouchIfCurrent(c.key, c.lease)
		}
	}
	if c.h.cacheRouter != nil {
		// TTFB includes normalization and queue time; remove pre-send time
		// before estimating upstream prefill throughput.
		first := st.ttfb - sent.Sub(st.start)
		if first < 0 {
			first = 0
		}
		c.h.cacheRouter.Observe(st.uid, c.trace, st.inputTokens, st.cacheRead, c.predicted, cacheKnown, first, time.Since(sent))
	}
}

func (c *chatRoute) log(st *chatStat) {
	if c.h.cacheRouter == nil {
		return
	}
	active, hot := c.h.cfg.Pool.RouteActivity(st.uid)
	hash := c.lease.Hash
	if hash == "" {
		hash = c.trace.Root
	}
	log.Printf("[cache_route] response_id=%q key_source=%s key_hash=%s primary_hash=%s account_hash=%s binding_version=%d reason=%s shadow_account_hash=%s predicted_cache=%d actual_cache=%d cache_known=%t input_tokens=%d queue_ms=%d queue_result=%q active_sessions=%d hot_sessions=%d status=%d",
		st.id, c.source, hash, c.h.cacheRouter.Hash(c.lease.UID), c.h.cacheRouter.Hash(st.uid), c.lease.Version, c.reason, c.h.cacheRouter.Hash(c.shadowUID), c.predicted, st.cacheRead, c.cacheKnown, st.inputTokens, c.wait.Milliseconds(), c.queueReason, active, hot, st.status)
}
