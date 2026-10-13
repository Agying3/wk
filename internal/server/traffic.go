package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"workbuddy2api/internal/reqproxy"
	"workbuddy2api/internal/traffic"
	"workbuddy2api/internal/upstream"
)

type trafficResultKey struct{}
type trafficResult struct{ counters *traffic.Counters }

type trafficWriter struct {
	http.ResponseWriter
	status int
}

func (w *trafficWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *trafficWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *trafficWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}
func (w *trafficWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *Handler) observeClientRequest(next http.HandlerFunc, w http.ResponseWriter, r *http.Request) {
	scope := scopeFromRequest(r)
	id, name := scope.keyID, scope.keyName
	if id == "" {
		id, name = "console-session", "控制台会话"
		if scope.adminKey {
			id, name = "global-admin", "全局管理员 key"
		}
	}
	lease := h.cfg.Traffic.Begin(traffic.Entity{Dimension: "total", ID: "all", Name: "全站"},
		traffic.Entity{Dimension: "key", ID: id, Name: maskPhonesIn(name)})
	result := &trafficResult{}
	writer := &trafficWriter{ResponseWriter: w}
	started := time.Now()
	defer func() {
		c := traffic.Counters{Completed: 1, CreditUnknownRequests: 1, LatencyMillis: time.Since(started).Seconds() * 1000}
		if result.counters != nil {
			c = *result.counters
		} else if writer.status >= 200 && writer.status < 300 {
			c.Successes = 1
		} else {
			c.Failures = 1
		}
		lease.End(c)
	}()
	next(writer, r.WithContext(context.WithValue(r.Context(), trafficResultKey{}, result)))
}

func (h *Handler) beginTrafficAttempt(ctx context.Context, st *chatStat, uid, name string, started time.Time) context.Context {
	entities := []traffic.Entity{{Dimension: "account", ID: uid, Name: maskPhonesIn(name)}}
	if h.cfg.Groups != nil {
		for _, group := range h.cfg.Groups.AccountGroups(uid) {
			entities = append(entities, traffic.Entity{Dimension: "account_group", ID: group, Name: group})
		}
	}
	if len(entities) == 1 {
		entities = append(entities, traffic.Entity{Dimension: "account_group", ID: "default", Name: "default"})
	}
	st.trafficAttempt = h.cfg.Traffic.Begin(entities...)
	st.trafficAttemptStart = started
	return upstream.WithProxyObserver(ctx, func(proxy *url.URL) {
		id, name, groupID, groupName := "direct", "直连", "direct", "直连"
		if proxy != nil {
			hash := sha256.Sum256([]byte(proxy.Scheme + "://" + proxy.Host))
			id, name, groupID, groupName = fmt.Sprintf("external-%x", hash[:8]), "外部代理", "external", "外部代理"
			if h.cfg.ReqProxy != nil {
				port, _ := strconv.Atoi(proxy.Port())
				if nodeID, nodeName, pgID, pgName, ok := h.cfg.ReqProxy.RouteAtPort(port); ok {
					id, name, groupID, groupName = nodeID, nodeName, pgID, pgName
				}
			}
		}
		st.trafficProxyStart = time.Now()
		st.trafficProxy = h.cfg.Traffic.Begin(traffic.Entity{Dimension: "proxy", ID: id, Name: maskPhonesIn(name)},
			traffic.Entity{Dimension: "proxy_group", ID: groupID, Name: maskPhonesIn(groupName)})
	})
}

func (s *chatStat) finishTraffic(status int, elapsed time.Duration) {
	c := traffic.Counters{Completed: 1, InputTokens: int64(s.inputTokens), OutputTokens: int64(maxInt(s.toks, 0)), TotalTokens: int64(s.totalTokens),
		CacheReadTokens: int64(s.cacheRead), CacheWriteTokens: int64(s.cacheWrite), ToolCalls: int64(s.toolCalls), LatencyMillis: elapsed.Seconds() * 1000}
	if s.trafficCacheKnown {
		c.CacheInputTokens = c.InputTokens
		c.CacheSamples = 1
	}
	if status >= 200 && status < 300 {
		c.Successes = 1
	} else {
		c.Failures = 1
	}
	if s.ttfb > 0 {
		c.TTFBSamples = 1
		c.TTFBMillis = s.ttfb.Seconds() * 1000
	}
	switch s.creditSource {
	case "upstream":
		c.CreditsUpstream = s.creditsConsumed
	case "estimated":
		c.CreditsEstimated = s.creditsConsumed
	default:
		c.CreditUnknownRequests = 1
	}
	if s.trafficResult != nil {
		copy := c
		s.trafficResult.counters = &copy
	}
	s.finishTrafficAttempt(c)
}

func (s *chatStat) finishTrafficAttempt(c traffic.Counters) {
	if s.trafficAttempt == nil {
		return
	}
	now := time.Now()
	// Account/proxy latency excludes queueing and credential maintenance. Their
	// TTFB starts at the model attempt, unlike end-to-end client TTFB.
	c.LatencyMillis = now.Sub(s.trafficAttemptStart).Seconds() * 1000
	if c.TTFBSamples > 0 {
		c.TTFBMillis = max(c.TTFBMillis-s.trafficAttemptStart.Sub(s.start).Seconds()*1000, 0)
	}
	s.trafficAttempt.End(c)
	if s.trafficProxy != nil {
		c.LatencyMillis = now.Sub(s.trafficProxyStart).Seconds() * 1000
		if c.TTFBSamples > 0 {
			c.TTFBMillis = max(c.TTFBMillis-s.trafficProxyStart.Sub(s.trafficAttemptStart).Seconds()*1000, 0)
		}
		s.trafficProxy.End(c)
	}
	s.trafficAttempt, s.trafficProxy = nil, nil
}

var trafficWindows = map[string]time.Duration{"1m": time.Minute, "5m": 5 * time.Minute, "1h": time.Hour, "6h": 6 * time.Hour,
	"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": traffic.Retention}

func (h *Handler) trafficStats(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "24h"
	}
	duration, ok := trafficWindows[window]
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_window", "window must be 1m, 5m, 1h, 6h, 24h, 7d or 30d")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := h.cfg.Traffic.Snapshot(ctx, duration)
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "traffic_unavailable", "traffic history is temporarily unavailable")
		return
	}
	// Include idle objects, and retain archived historical objects as well.
	known := map[string]bool{}
	for _, row := range result.Rows {
		known[row.Dimension+"\x00"+row.ID] = true
	}
	add := func(dim, id, name string) {
		k := dim + "\x00" + id
		if known[k] {
			return
		}
		known[k] = true
		result.Rows = append(result.Rows, traffic.Row{Entity: traffic.Entity{Dimension: dim, ID: id, Name: maskPhonesIn(name)}})
	}
	add("total", "all", "全站")
	add("proxy", "direct", "直连")
	add("proxy_group", "direct", "直连")
	if h.cfg.APIKey != "" {
		add("key", "global-admin", "全局管理员 key")
	}
	if h.cfg.Pool != nil {
		for _, a := range h.cfg.Pool.List() {
			add("account", a.UID, a.Nickname)
		}
	}
	if h.cfg.Groups != nil {
		for _, group := range h.cfg.Groups.List() {
			add("account_group", group, group)
		}
		for _, key := range h.cfg.Groups.ListKeys() {
			add("key", key.ID, key.Name)
		}
	} else {
		add("account_group", "default", "default")
	}
	if h.cfg.ReqProxy != nil {
		for _, node := range h.cfg.ReqProxy.NodesSnapshot() {
			add("proxy", node.ID, node.Name)
		}
		h.cfg.ReqProxy.View(func(st *reqproxy.State) {
			for _, group := range st.ProxyGroups {
				add("proxy_group", group.ID, group.Name)
			}
		})
	}
	sort.Slice(result.Rows, func(i, j int) bool {
		a, b := result.Rows[i], result.Rows[j]
		if a.Dimension != b.Dimension {
			return a.Dimension < b.Dimension
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return a.ID < b.ID
	})
	writeJSON(w, http.StatusOK, result)
}
