package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/coordination"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
)

type failedRouteAuthority struct{}

func (failedRouteAuthority) Update(context.Context, string, func(json.RawMessage) (json.RawMessage, error)) error {
	return errors.New("offline authority")
}
func (failedRouteAuthority) View(context.Context, string) (json.RawMessage, error) {
	return nil, errors.New("offline authority")
}

func TestRouteSessionAuthorityFailureDoesNotFallback(t *testing.T) {
	p := routingPool()
	h := newTestHandler(t, Config{Pool: p, Session: session.New(session.Config{Salt: []byte("offline-test-salt"), Authority: failedRouteAuthority{}})})
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	route := h.beginChatRoute(r, "conversation", "explicit", "glm-5.2", nil, nil)
	if a, err := route.acquire(r.Context(), nil, nil); err == nil || a != nil {
		t.Fatal("failed shared binding authority fell back to a local account")
	}
}

func TestRouteSharedLeaseReleaseAndAuthorityFence(t *testing.T) {
	dsn := os.Getenv("WB2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("WB2API_TEST_POSTGRES_DSN is required")
	}
	store, err := coordination.OpenPostgres(context.Background(), dsn, fmt.Sprintf("route-test/%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "A", FilePath: "offline-A.json"})
	p.SetCredits("A", 1000)
	p.SetMaxInFlight(2)
	if err = p.EnableShared(context.Background(), store, "live", func(string) (bool, error) { return true, nil }, true); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: Config{Pool: p}}
	first := &chatRoute{h: h, model: "m"}
	second := &chatRoute{h: h, model: "m"}
	a, err := first.acquire(context.Background(), nil, nil)
	if err != nil || a == nil {
		t.Fatal("first route", err)
	}
	a, err = second.acquire(context.Background(), nil, nil)
	if err != nil || a == nil {
		t.Fatal("second route", err)
	}
	first.release("A")
	first.release("A")
	st, _ := p.Status("A")
	if st.InFlight != 1 {
		t.Fatal("route release decremented another request", st.InFlight)
	}
	second.release("A")
	_ = store.Close()
	third := &chatRoute{h: h, model: "m"}
	if a, err = third.acquire(context.Background(), nil, nil); err == nil || a != nil {
		t.Fatal("route hid shared pool failure")
	}
}
