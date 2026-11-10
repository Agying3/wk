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
	authorityErr       error
	poolLeases         map[string]pool.Lease
}

func (c *chatRoute) admissionEnabled() bool {
	return c.h.cacheRouter != nil && !c.h.cacheRouter.Config().Shadow
}
func (c *chatRoute) request(related bool) pool.RouteRequest {
	return pool.RouteRequest{Tenant: c.tenant, Session: c.key, Prefix: c.trace.Root, Related: related}
}
func (c *chatRoute) pickUID(uid string, related bool, tried, allow map[string]bool) *auth.Auth {
	if c.authorityErr != nil {
		return nil
	}
	if p := c.h.cfg.Pool; p.SharedEnabled() {
		a, l, err := p.AcquireShared(c.model, c.request(related), tried, allow, uid, c.admissionEnabled())
		c.authorityErr = err
		if a != nil {
			if c.poolLeases == nil {
				c.poolLeases = map[string]pool.Lease{}
			}
			c.poolLeases[a.UID] = l
		}
		return a
	}
	if c.admissionEnabled() {
		return c.h.cfg.Pool.PickAndAcquireByUIDForRoute(uid, c.model, c.request(related), tried, allow)
	}
	return c.h.cfg.Pool.PickAndAcquireByUIDForModel(uid, c.model)
}
func (c *chatRoute) release(uid string) {
	if p := c.h.cfg.Pool; p.SharedEnabled() {
		if l, ok := c.poolLeases[uid]; ok {
			delete(c.poolLeases, uid)
			if err := p.ReleaseLease(l); err != nil {
				c.authorityErr = err
				log.Printf("[cache_route] shared lease release failed")
			}
		}
		return
	}
	if c.admissionEnabled() {
		c.h.cfg.Pool.ReleaseRoute(uid, c.request(false))
	} else {
		c.h.cfg.Pool.Release(uid)
	}
}

func routeNamespace(r *http.Request) string {
	s := scopeFromRequest(r)
	return fmt.Sprintf("%q/%q/%q", s.kind, s.group, s.keyID)
}

func (h *Handler) beginResolvedChatRoute(r *http.Request, identity session.Identity, model string, doc map[string]any, allow map[string]bool) *chatRoute {
	c := &chatRoute{h: h, source: identity.Source, model: model, reason: "rotation"}
	preferred := ""
	states := h.cfg.Pool.RoutingStates(model, allow)
	if c.authorityErr = h.cfg.Pool.SharedError(); c.authorityErr != nil {
		return c
	}
	if h.cacheRouter != nil {
		c.tenant = h.cacheRouter.Hash(routeNamespace(r))
		c.trace = h.cacheRouter.Build(routeNamespace(r), model, doc)
		if plans := h.cacheRouter.Plans("", c.trace, states, nil); len(plans) > 0 && !h.cacheRouter.Config().Shadow {
			preferred = plans[0].UID
		}
	}
	if identity.Empty() || h.cfg.Session == nil || identity.Source == "prefix" && (h.cacheRouter == nil || h.cacheRouter.Config().Shadow) {
		return c
	}
	c.key = identity.AccountKey(model)
	if _, exists := h.cfg.Session.LookupLease(c.key); !exists {
		legacyKey := identity.LegacyRaw()
		if identity.Source == "prefix" {
			legacyKey = session.ScopedKey(routeNamespace(r), model, "prefix:"+c.trace.Root)
		}
		if legacy, ok := h.cfg.Session.LookupLease(legacyKey); ok {
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
	c.lease, _, c.authorityErr = h.cfg.Session.ResolveLeaseWithCandidatesChecked(c.key, allow, preferred, healthy, available)
	return c
}

func (c *chatRoute) acquire(ctx context.Context, tried, allow map[string]bool) (*auth.Auth, error) {
	if c.authorityErr != nil {
		return nil, c.authorityErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := c.h.cfg.Pool
	states := p.RoutingStates(c.model, allow)
	if err := p.SharedError(); err != nil {
		return nil, err
	}
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
				if c.authorityErr != nil {
					return nil, c.authorityErr
				}
				if acct == nil && remaining > 0 {
					start := time.Now()
					waitCtx, cancel := context.WithTimeout(ctx, remaining)
					var waitErr error
					if p.SharedEnabled() {
						var lease pool.Lease
						acct, lease, waitErr = p.WaitAcquireShared(waitCtx, plan.UID, c.model, cfg.MaxPending, cfg.MaxPendingTotal, c.request(plan.CacheTokens >= 512), tried, allow)
						if acct != nil {
							if c.poolLeases == nil {
								c.poolLeases = map[string]pool.Lease{}
							}
							c.poolLeases[acct.UID] = lease
						}
					} else {
						acct, waitErr = p.WaitAndAcquireByUIDForRoute(waitCtx, plan.UID, c.model, cfg.MaxPending, cfg.MaxPendingTotal, c.request(plan.CacheTokens >= 512), tried, allow)
					}
					if waitErr != nil {
						c.queueReason = waitErr.Error()
					} else {
						c.queueReason = "slot_released"
					}
					cancel()
					if err := p.SharedError(); err != nil {
						return nil, err
					}
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
		if c.authorityErr != nil {
			return nil, c.authorityErr
		}
		if p.SharedEnabled() {
			var lease pool.Lease
			acct, lease, c.authorityErr = p.AcquireShared(c.model, c.request(false), tried, allow, "", c.admissionEnabled() && !c.h.cacheRouter.Config().ProtectionDisabled)
			if acct != nil {
				if c.poolLeases == nil {
					c.poolLeases = map[string]pool.Lease{}
				}
				c.poolLeases[acct.UID] = lease
			}
		} else if c.admissionEnabled() && !c.h.cacheRouter.Config().ProtectionDisabled {
			acct = p.PickAndAcquireRoute(c.model, c.request(false), tried, allow)
		} else {
			acct = p.PickAndAcquireForModelAllow(c.model, tried, allow)
		}
		if acct != nil {
			c.reason = "availability_fallback"
		}
	}
	if acct == nil {
		return nil, c.authorityErr
	}
	if err := ctx.Err(); err != nil {
		c.release(acct.UID)
		return nil, err
	}
	if c.key != "" && c.lease.UID != "" && acct.UID != c.lease.UID {
		healthy := p.HealthyForModel(c.lease.UID, c.model) && (allow == nil || allow[c.lease.UID])
		if err := p.SharedError(); err != nil {
			c.release(acct.UID)
			return nil, err
		}
		if !healthy {
			lease, ok, err := c.h.cfg.Session.ReplaceIfCurrentChecked(c.key, c.lease, acct.UID)
			if err != nil {
				c.release(acct.UID)
				return nil, err
			}
			if ok {
				c.lease = lease
				c.reason = "primary_unavailable"
			}
		} else if c.reason == "prefix" {
			c.reason = "warm_borrow"
		}
	}
	if err := p.SharedError(); err != nil {
		c.release(acct.UID)
		return nil, err
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
