package reqproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/coordination"
)

func proxyCoordinator(t *testing.T) *coordination.Store {
	t.Helper()
	dsn := os.Getenv("WB2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set WB2API_TEST_POSTGRES_DSN to an isolated PostgreSQL database")
	}
	s, err := coordination.OpenPostgres(context.Background(), dsn, fmt.Sprintf("reqproxy-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSharedProxyBindingsAndLocalKernelIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("uses local Xray kernels and isolated PostgreSQL")
	}
	s := proxyCoordinator(t)
	up := fakeUpstream(t, "same-authoritative-exit")
	defer up.Close()
	node := httpNodeSpec("shared-node", strings.TrimPrefix(up.URL, "http://"))
	st := DefaultState()
	st.Enabled = true
	st.Rules.AccountsPerNode = 1
	err := s.Update(context.Background(), "reqproxy/runtime", func(json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(sharedSnapshot{State: st, Nodes: map[string]*Node{node.ID: {NodeSpec: node, Probed: true}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	newRelay := func(ip string) *Manager {
		k, e := NewKernelAt(ip)
		if e != nil {
			t.Fatal(e)
		}
		cfg := DefaultConfig()
		cfg.StateFile = filepath.Join(t.TempDir(), "proxy.json")
		cfg.PortMin = 32840
		cfg.PortMax = 32890
		m, e := NewSharedManager(cfg, k, s, ip, false)
		if e != nil {
			k.Close()
			t.Fatal(e)
		}
		t.Cleanup(m.Close)
		return m
	}
	a, b := newRelay("127.0.0.21"), newRelay("127.0.0.22")
	ua, err := a.DialProxy("same-account", "cn")
	if err != nil {
		t.Fatal(err)
	}
	ub, err := b.DialProxy("same-account", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if ua.Port() == "" || ub.Port() == "" || ua.Hostname() == ub.Hostname() {
		t.Fatalf("binding or local address differs: %s %s", ua, ub)
	}
	for _, proxy := range []string{ua.String(), ub.String()} {
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(mustParseURL(proxy))}}
		resp, e := client.Get(up.URL)
		if e != nil {
			t.Fatal(e)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		client.CloseIdleConnections()
		if string(body) != "same-authoritative-exit" {
			t.Fatalf("relay lost original exit: %s", body)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := a
			if i%2 != 0 {
				m = b
			}
			if _, e := m.DialProxy(fmt.Sprintf("account-%d", i), "cn"); e != nil {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	_, bindings := a.Views()
	if len(bindings) != 13 {
		t.Fatalf("overlapping runtime lost bindings: %d", len(bindings))
	}
	// A store-only settings mutation must retain slots allocated by B.
	a.Update(func(st *State) { st.Rules.MaxLatencyMs = 12345 })
	_, bindings = b.Views()
	if len(bindings) != 13 {
		t.Fatal("owner settings overwrote relay bindings")
	}
	if err = b.FlushChecked(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for i := 0; i < 2; i++ {
		if proxy, e := b.DialProxy("same-account", "cn"); e == nil || proxy != nil {
			t.Fatal("unavailable authority returned a cached route")
		}
	}
	if b.Ready() {
		t.Fatal("fenced proxy remained ready")
	}
}

func TestKernelRejectsNonLoopbackListen(t *testing.T) {
	for _, address := range []string{"0.0.0.0", "192.0.2.1", "::1", "invalid"} {
		if err := validateListenIP(address); err == nil {
			t.Fatalf("accepted %q", address)
		}
	}
}
