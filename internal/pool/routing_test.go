package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func awaitQueue(t *testing.T, p *Pool, n int) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		p.mu.RLock()
		got := p.totalWaiters
		p.mu.RUnlock()
		if got == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue did not reach %d", n)
}

func TestWaitQueueFIFOAndNoSelectionOvertake(t *testing.T) {
	p := modelTestPool(&auth.Auth{UID: "A"})
	p.SetMaxInFlight(1)
	p.Acquire("A")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { _, err := p.WaitAndAcquireByUIDForModel(ctx, "A", "m", 2, 2); first <- err }()
	awaitQueue(t, p, 1)
	go func() { _, err := p.WaitAndAcquireByUIDForModel(ctx, "A", "m", 2, 2); second <- err }()
	awaitQueue(t, p, 2)
	p.Release("A")
	if a := p.PickAndAcquireByUIDForModel("A", "m"); a != nil {
		p.Release("A")
		t.Fatal("ordinary selector jumped queue")
	}
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-second:
		t.Fatal("second acquired occupied slot")
	default:
	}
	p.Release("A")
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	p.Release("A")
	awaitQueue(t, p, 0)
	state, _ := p.Status("A")
	if state.InFlight != 0 {
		t.Fatal("lease leaked")
	}
}

func TestWaitQueueCancellationAndBounds(t *testing.T) {
	p := modelTestPool(&auth.Auth{UID: "A"}, &auth.Auth{UID: "B"})
	p.SetMaxInFlight(1)
	p.Acquire("A")
	p.Acquire("B")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := p.WaitAndAcquireByUIDForModel(ctx, "A", "m", 1, 1); done <- err }()
	awaitQueue(t, p, 1)
	for _, uid := range []string{"A", "B"} {
		if _, err := p.WaitAndAcquireByUIDForModel(context.Background(), uid, "m", 1, 1); !errors.Is(err, ErrWaitQueueFull) {
			t.Fatalf("%s: %v", uid, err)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	awaitQueue(t, p, 0)
	p.Release("A")
	p.Release("B")
	if a := p.PickAndAcquireByUIDForModel("A", "m"); a == nil {
		t.Fatal("cancelled waiter still blocked account")
	} else {
		p.Release("A")
	}
}

func TestWaitQueueTimeoutAndModelInvalidation(t *testing.T) {
	p := modelTestPool(&auth.Auth{UID: "A"})
	p.SetMaxInFlight(1)
	p.Acquire("A")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if _, err := p.WaitAndAcquireByUIDForModel(ctx, "A", "m", 2, 4); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	awaitQueue(t, p, 0)
	p.CooldownModel("A", "m", time.Minute, "test")
	if _, err := p.WaitAndAcquireByUIDForModel(context.Background(), "A", "m", 2, 4); !errors.Is(err, ErrAccountUnavailable) {
		t.Fatal(err)
	}
	p.Release("A")
	if a := p.PickAndAcquireByUIDForModel("A", "other"); a == nil {
		t.Fatal("unrelated model was blocked")
	} else {
		p.Release("A")
	}
}
