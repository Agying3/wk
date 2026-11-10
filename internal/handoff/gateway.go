// Package handoff keeps client connections open across single-writer releases.
package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Target, Release         string
	MaxPending, MaxActive   int
	MaxBody, MaxQueuedBytes int64
	Wait                    time.Duration
	TrustIngress            bool
	StatePath               string
	Overlap                 bool
}
type waiter struct {
	ready      chan struct{}
	target     *url.URL
	bytes      int64
	dispatched bool
	release    string
	management bool
}
type releaseTarget struct {
	Target           string `json:"target"`
	RelayActive      int    `json:"relay_active"`
	ManagementActive int    `json:"management_active"`
	Unknown          bool   `json:"unknown,omitempty"`
}
type Gateway struct {
	mu                sync.Mutex
	cfg               Config
	target            *url.URL
	release           string
	held              bool
	active            int
	bytes             int64
	queue             []*waiter
	proxy             *httputil.ReverseProxy
	managementTarget  *url.URL
	managementRelease string
	managementHeld    bool
	recoveryRequired  bool
	transition        bool
	releases          map[string]*releaseTarget
	syncStateDir      func(string) error
}

func localTarget(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("target must be a loopback HTTP origin")
	}
	ip := net.ParseIP(u.Hostname())
	port, e := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || e != nil || port < 1 || port > 65535 {
		return nil, errors.New("target must use a loopback IP and explicit port")
	}
	return u, nil
}
func New(c Config) (*Gateway, error) {
	held := false
	var restored gatewayState
	if c.StatePath != "" {
		raw, err := os.ReadFile(c.StatePath)
		if err == nil {
			var s gatewayState
			if json.Unmarshal(raw, &s) != nil || (s.Version != 1 && s.Version != 2) || s.Release == "" {
				return nil, errors.New("invalid gateway state")
			}
			c.Target = s.Target
			c.Release = s.Release
			held = s.Held
			restored = s
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	u, e := localTarget(c.Target)
	if e != nil {
		return nil, e
	}
	if c.Release == "" || c.MaxPending < 1 || c.MaxActive < 1 || c.MaxBody < 1 || c.MaxQueuedBytes < c.MaxBody || c.Wait <= 0 {
		return nil, errors.New("invalid gateway limits or release")
	}
	g := &Gateway{cfg: c, target: u, release: c.Release, held: held, managementTarget: u, managementRelease: c.Release, releases: map[string]*releaseTarget{c.Release: {Target: u.String()}}}
	if restored.Version == 2 {
		if !c.Overlap {
			return nil, errors.New("overlap state requires overlap mode")
		}
		management, err := localTarget(restored.ManagementTarget)
		if err != nil || restored.ManagementRelease == "" {
			return nil, errors.New("invalid management target")
		}
		g.managementTarget, g.managementRelease, g.managementHeld = management, restored.ManagementRelease, restored.ManagementHeld
		g.transition = restored.Transition
		for release, target := range restored.Retired {
			if release == "" || release == c.Release {
				return nil, errors.New("invalid retired release")
			}
			if _, err := localTarget(target); err != nil {
				return nil, err
			}
			g.releases[release] = &releaseTarget{Target: target, Unknown: true}
		}
		if state, ok := g.releases[g.managementRelease]; !ok || state.Target != management.String() {
			return nil, errors.New("management target does not match release state")
		}
		// Request counts cannot survive a gateway crash. Never interpret lost
		// counters as proof that an old backend has drained.
		if g.transition || len(restored.Retired) > 0 {
			g.recoveryRequired, g.held, g.managementHeld = true, true, true
		}
	}
	if err := g.persist(); err != nil {
		return nil, err
	}
	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(p.In.Context().Value(targetKey{}).(*url.URL))
			p.Out.Host = p.In.Host
			if c.TrustIngress {
				p.Out.Header["X-Forwarded-For"] = append([]string(nil), p.In.Header.Values("X-Forwarded-For")...)
			}
			p.SetXForwarded()
			if c.TrustIngress {
				proto := p.In.Header.Get("X-Forwarded-Proto")
				if proto == "https" || proto == "http" {
					p.Out.Header.Set("X-Forwarded-Proto", proto)
				}
			}
		},
		FlushInterval: -1,
		// Never retry a request whose upstream execution may already have begun.
		Transport:    &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, MaxIdleConnsPerHost: 32},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) { http.Error(w, "backend unavailable", 502) },
	}
	return g, nil
}

type gatewayState struct {
	Version                             int
	Target, Release                     string
	Held                                bool
	ManagementTarget, ManagementRelease string            `json:",omitempty"`
	ManagementHeld, Transition          bool              `json:",omitempty"`
	Retired                             map[string]string `json:",omitempty"`
}

func (g *Gateway) snapshot() gatewayState {
	s := gatewayState{Version: 1, Target: g.target.String(), Release: g.release, Held: g.held}
	if g.cfg.Overlap {
		s.Version, s.ManagementTarget, s.ManagementRelease, s.ManagementHeld, s.Transition = 2, g.managementTarget.String(), g.managementRelease, g.managementHeld, g.transition
		s.Retired = map[string]string{}
		for release, state := range g.releases {
			if release != g.release {
				s.Retired[release] = state.Target
			}
		}
	}
	return s
}

type stateCommitError struct {
	committed bool
	err       error
}

func (e *stateCommitError) Error() string { return e.err.Error() }
func (e *stateCommitError) Unwrap() error { return e.err }

func (g *Gateway) persist() error {
	if g.cfg.StatePath == "" {
		return nil
	}
	raw, err := json.Marshal(g.snapshot())
	if err != nil {
		return err
	}
	f, err := os.OpenFile(g.cfg.StatePath+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(g.cfg.StatePath+".tmp", g.cfg.StatePath); err != nil {
		return err
	}
	syncDir := g.syncStateDir
	if syncDir == nil {
		syncDir = syncDirectory
	}
	if err := syncDir(filepath.Dir(g.cfg.StatePath)); err != nil {
		return &stateCommitError{committed: true, err: err}
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type targetKey struct{}

func (g *Gateway) dispatch() {
	for i := 0; !g.held && !g.recoveryRequired && g.active < g.cfg.MaxActive && i < len(g.queue); {
		q := g.queue[i]
		if q.management && g.managementHeld {
			i++
			continue
		}
		g.queue = append(g.queue[:i], g.queue[i+1:]...)
		g.bytes -= q.bytes
		q.target, q.release = g.target, g.release
		if q.management {
			q.target, q.release = g.managementTarget, g.managementRelease
		}
		q.dispatched = true
		g.active++
		g.track(q.release, q.management, 1)
		close(q.ready)
	}
}
func (g *Gateway) track(release string, management bool, delta int) {
	s := g.releases[release]
	if management {
		s.ManagementActive += delta
	} else {
		s.RelayActive += delta
	}
}
func (g *Gateway) leave(release string, management bool) {
	g.mu.Lock()
	g.active--
	g.track(release, management, -1)
	g.dispatch()
	g.mu.Unlock()
}
func (g *Gateway) acquire(ctx context.Context, size int64) (*url.URL, error) {
	q, err := g.acquireRoute(ctx, size, false)
	if err != nil {
		return nil, err
	}
	return q.target, nil
}
func (g *Gateway) acquireRoute(ctx context.Context, size int64, management bool) (*waiter, error) {
	g.mu.Lock()
	if g.recoveryRequired {
		g.mu.Unlock()
		return nil, errors.New("upgrade recovery required")
	}
	waiting := false
	for _, pending := range g.queue {
		if !pending.management || !g.managementHeld {
			waiting = true
			break
		}
	}
	if !g.held && (!management || !g.managementHeld) && g.active < g.cfg.MaxActive && !waiting {
		g.active++
		q := &waiter{target: g.target, release: g.release, management: management, dispatched: true}
		if management {
			q.target, q.release = g.managementTarget, g.managementRelease
		}
		g.track(q.release, management, 1)
		g.mu.Unlock()
		return q, nil
	}
	if len(g.queue) >= g.cfg.MaxPending || size > g.cfg.MaxQueuedBytes-g.bytes {
		g.mu.Unlock()
		return nil, errors.New("upgrade queue full")
	}
	q := &waiter{ready: make(chan struct{}), bytes: size, management: management}
	g.queue = append(g.queue, q)
	g.bytes += size
	g.dispatch()
	g.mu.Unlock()
	t := time.NewTimer(g.cfg.Wait)
	defer t.Stop()
	var err error
	select {
	case <-q.ready:
		return q, nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-t.C:
		err = errors.New("upgrade queue timeout")
	}
	g.mu.Lock()
	if q.dispatched {
		g.active--
		g.track(q.release, q.management, -1)
	} else {
		for i, v := range g.queue {
			if v == q {
				g.queue = append(g.queue[:i], g.queue[i+1:]...)
				g.bytes -= q.bytes
				break
			}
		}
	}
	g.dispatch()
	g.mu.Unlock()
	return nil, err
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > g.cfg.MaxBody {
		http.Error(w, "request too large", 413)
		return
	}
	// Do not read or spool queued bodies. Reserving the maximum for unknown
	// lengths and bounding headers/connections keeps admission memory bounded.
	size := r.ContentLength
	if size < 0 {
		size = g.cfg.MaxBody
	}
	size += 32768
	management := g.cfg.Overlap && !IsRelayPath(r.URL.Path)
	q, err := g.acquireRoute(r.Context(), size, management)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, err.Error(), 503)
		return
	}
	defer g.leave(q.release, management)
	if r.Context().Err() != nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, g.cfg.MaxBody)
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, q.target)))
}

// IsRelayPath defines the stable ingress data plane. Everything else remains
// with the management owner until its explicit handoff.
func IsRelayPath(path string) bool {
	return strings.HasPrefix(path, "/v1/") || path == "/chat/completions" || path == "/responses" || path == "/models" || strings.HasPrefix(path, "/models/") || path == "/readyz" || path == "/livez" || path == "/healthz" || path == "/legal" || strings.HasPrefix(path, "/legal/")
}

// Control must only be bound to a private Unix socket. Expected release is a
// compare-and-swap precondition, not an authentication credential.
func (g *Gateway) Control() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		status := map[string]any{"release": g.release, "held": g.held, "active": g.active, "pending": len(g.queue), "queued_bytes": g.bytes, "wait_seconds": g.cfg.Wait.Seconds(), "mode": "serial"}
		if g.cfg.Overlap {
			status["mode"], status["overlap_protocol"] = "overlap", 1
			status["management_release"], status["management_held"] = g.managementRelease, g.managementHeld
			status["releases"], status["recovery_required"] = g.releases, g.recoveryRequired
			status["transition"] = g.transition
		}
		_ = json.NewEncoder(w).Encode(status)
	})
	m.HandleFunc("POST /{action}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		action := r.PathValue("action")
		expected := g.release
		if action == "hold-management" || action == "switch-management" || action == "resume-management" {
			expected = g.managementRelease
		}
		if r.URL.Query().Get("expected") != expected {
			http.Error(w, "release mismatch", 409)
			return
		}
		oldTarget, oldRelease, oldHeld := g.target, g.release, g.held
		oldManagementTarget, oldManagementRelease, oldManagementHeld, oldTransition, oldRecovery := g.managementTarget, g.managementRelease, g.managementHeld, g.transition, g.recoveryRequired
		oldReleases := make(map[string]*releaseTarget, len(g.releases))
		for key, value := range g.releases {
			oldReleases[key] = value
		}
		if g.recoveryRequired && action != "retire" && action != "resume" && action != "hold-management" && action != "switch-management" {
			http.Error(w, "upgrade recovery required", 409)
			return
		}
		switch action {
		case "hold":
			g.held = true
		case "resume":
			if g.recoveryRequired {
				unknown := false
				for _, release := range g.releases {
					unknown = unknown || release.Unknown
				}
				if r.URL.Query().Get("confirm_recovery") != "true" || unknown || g.managementRelease != g.release {
					http.Error(w, "upgrade recovery required", 409)
					return
				}
				g.recoveryRequired = false
			}
			g.held = false
		case "switch":
			if !g.held || g.active != 0 || (g.cfg.Overlap && (g.transition || len(g.releases) != 1)) {
				http.Error(w, "not drained", 409)
				return
			}
			u, err := localTarget(r.URL.Query().Get("target"))
			next := r.URL.Query().Get("next")
			if err != nil || next == "" || next == g.release {
				http.Error(w, "invalid target or release", 400)
				return
			}
			g.target = u
			g.release = next
			g.managementTarget, g.managementRelease = u, next
			g.releases = map[string]*releaseTarget{next: {Target: u.String()}}
		case "switch-live":
			if !g.cfg.Overlap || g.held || g.managementHeld || g.managementRelease != g.release || len(g.releases) != 1 || g.transition {
				http.Error(w, "overlap switch unavailable", 409)
				return
			}
			u, err := localTarget(r.URL.Query().Get("target"))
			next := r.URL.Query().Get("next")
			if err != nil || next == "" || next == g.release || u.String() == g.target.String() {
				http.Error(w, "invalid target or release", 400)
				return
			}
			g.target, g.release, g.transition = u, next, true
			g.releases[next] = &releaseTarget{Target: u.String()}
		case "rollback-live":
			next := r.URL.Query().Get("next")
			retired, ok := g.releases[next]
			if !g.cfg.Overlap || g.held || !ok || retired.Unknown || next == g.release || g.managementRelease != next {
				http.Error(w, "live rollback unavailable", 409)
				return
			}
			u, err := localTarget(retired.Target)
			if err != nil {
				http.Error(w, "invalid retired target", 409)
				return
			}
			g.target, g.release = u, next
		case "hold-management":
			if !g.cfg.Overlap {
				http.Error(w, "overlap unavailable", 409)
				return
			}
			g.managementHeld = true
		case "resume-management":
			if !g.cfg.Overlap {
				http.Error(w, "overlap unavailable", 409)
				return
			}
			g.managementHeld = false
		case "switch-management":
			if !g.cfg.Overlap || !g.managementHeld || g.releases[g.managementRelease].ManagementActive != 0 || (g.releases[g.managementRelease].Unknown && r.URL.Query().Get("confirmed_drained") != "true") {
				http.Error(w, "management not drained", 409)
				return
			}
			next := r.URL.Query().Get("next")
			if next != g.release {
				http.Error(w, "management must use active release", 409)
				return
			}
			g.managementTarget, g.managementRelease = g.target, next
		case "retire":
			retired := r.URL.Query().Get("release")
			state, ok := g.releases[retired]
			if !g.cfg.Overlap || !ok || retired == g.release || retired == g.managementRelease || state.RelayActive != 0 || state.ManagementActive != 0 || (state.Unknown && r.URL.Query().Get("confirmed_drained") != "true") {
				http.Error(w, "release not drained", 409)
				return
			}
			delete(g.releases, retired)
		case "complete":
			if !g.cfg.Overlap || len(g.releases) != 1 || g.managementRelease != g.release || g.managementHeld {
				http.Error(w, "overlap not complete", 409)
				return
			}
			g.transition = false
		default:
			http.Error(w, "unknown action", 404)
			return
		}
		if err := g.persist(); err != nil {
			var commit *stateCommitError
			if errors.As(err, &commit) && commit.committed {
				g.recoveryRequired, g.held, g.managementHeld = true, true, true
			} else {
				g.target, g.release, g.held = oldTarget, oldRelease, oldHeld
				g.managementTarget, g.managementRelease, g.managementHeld, g.transition = oldManagementTarget, oldManagementRelease, oldManagementHeld, oldTransition
				g.recoveryRequired = oldRecovery
				g.releases = oldReleases
			}
			http.Error(w, "state commit failed; inspect gateway state before recovery", 500)
			return
		}
		g.dispatch()
		w.WriteHeader(204)
	})
	return m
}
