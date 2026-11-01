package upstream

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestRefreshCoalescesCredentialCopies(t *testing.T) {
	var requests atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			close(started)
		}
		<-release
		return jsonResp(200, `{"code":0,"data":{"accessToken":"new","refreshToken":"new-r"}}`), nil
	})
	var wg sync.WaitGroup
	accounts := make([]*auth.Auth, 24)
	shared := &auth.Auth{UID: "u", AccessToken: "old", RefreshToken: "r"}
	errs := make(chan error, len(accounts))
	for i := range accounts {
		accounts[i] = &auth.Auth{UID: "u", AccessToken: "old", RefreshToken: "r"}
		if i%2 == 0 {
			accounts[i] = shared
		}
		wg.Add(1)
		go func(a *auth.Auth) { defer wg.Done(); errs <- c.RefreshToken(a) }(accounts[i])
	}
	<-started
	// Separate Auth copies retain their old tokens even if scheduled after the
	// leader starts. Give all overlapping callers time to join the shared flight.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("refresh requests=%d", requests.Load())
	}
	for _, a := range accounts {
		if a.Snapshot().AccessToken != "new" || a.Snapshot().RefreshToken != "new-r" {
			t.Fatal("waiter did not receive rotated credentials")
		}
	}
}

func TestRefreshDifferentAuthoritiesDoNotShare(t *testing.T) {
	started, release := make(chan struct{}, 2), make(chan struct{})
	c := testClient(func(r *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-release
		return jsonResp(200, `{"code":0,"data":{"accessToken":"new"}}`), nil
	})
	done := make(chan error, 2)
	for _, enterprise := range []string{"a", "b"} {
		a := &auth.Auth{UID: "u", EnterpriseID: enterprise, AccessToken: "old", RefreshToken: "same"}
		go func() { done <- c.RefreshToken(a) }()
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for range 2 {
		select {
		case <-started:
		case <-timer.C:
			close(release)
			t.Fatal("different authorities merged")
		}
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestRefreshWhileMaintenanceLockHeld(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"new"}}`), nil
	})
	a := &auth.Auth{UID: "u", AccessToken: "old", RefreshToken: "r"}
	a.Lock()
	defer a.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.RefreshTokenContext(ctx, a); err != nil {
		t.Fatalf("refresh waited for maintenance lock: %v", err)
	}
}

func TestRefreshCanceledWaiterAndNewerCredentials(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := testClient(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-release
		return jsonResp(200, `{"code":0,"data":{"accessToken":"stale"}}`), nil
	})
	a := &auth.Auth{UID: "u", AccessToken: "old", RefreshToken: "r"}
	done := make(chan error, 1)
	go func() { done <- c.RefreshToken(a) }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { waiter <- c.RefreshTokenContext(ctx, a) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-waiter; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error=%v", err)
	}
	a.ApplyRefresh("new-import", "new-import-r", "", 0)
	close(release)
	if err := <-done; err == nil {
		t.Fatal("stale refresh accepted")
	}
	if a.Snapshot().AccessToken != "new-import" {
		t.Fatal("refresh overwrote newer credentials")
	}
}

func TestRefreshIdentityPolicies(t *testing.T) {
	for _, tc := range []struct {
		name, mode, identity string
		status               int
		fail                 bool
	}{
		{"matching", "best_effort", `{"uid":"u","type":"personal","oneidAccountId":"oid"}`, 200, false},
		{"wrong uid", "best_effort", `{"uid":"other"}`, 200, true},
		{"wrong enterprise", "best_effort", `{"uid":"u","enterpriseId":"other"}`, 200, true},
		{"wrong authority", "best_effort", `{"uid":"u","oneidAccountId":"other"}`, 200, true},
		{"temporary failure", "best_effort", `{}`, 503, false},
		{"strict temporary failure", "strict", `{}`, 503, true},
		{"legacy shape", "best_effort", `{}`, 200, false},
		{"strict legacy shape", "strict", `{}`, 200, true},
		{"rejected token", "best_effort", `{}`, 401, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					return jsonResp(200, `{"code":0,"data":{"accessToken":"new","refreshToken":"rotated"}}`), nil
				}
				if r.URL.Path != "/v2/plugin/account" || r.Header.Get("Authorization") != "Bearer new" || r.Header.Get("X-Refresh-Token") != "" {
					t.Error("incorrect identity request")
				}
				return jsonResp(tc.status, `{"code":0,"data":`+tc.identity+`}`), nil
			})
			c.RefreshIdentityCheck = tc.mode
			a := &auth.Auth{UID: "u", AccessToken: "old", RefreshToken: "r", Authority: auth.Authority{OneIDAccountID: "oid"}}
			err := c.RefreshToken(a)
			if (err != nil) != tc.fail {
				t.Fatalf("error=%v wantFail=%t", err, tc.fail)
			}
			want := "new"
			if tc.fail {
				want = "old"
			}
			if a.Snapshot().AccessToken != want {
				t.Fatal("unexpected credential mutation")
			}
		})
	}
}

func TestRefreshGlobalSelectsMatchingAccount(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			return jsonResp(200, `{"code":0,"data":{"accessToken":"new"}}`), nil
		}
		if r.URL.Path != "/v2/plugin/accounts" {
			t.Error("incorrect global endpoint")
		}
		return jsonResp(200, `{"code":0,"data":{"accounts":[{"uid":"other"},{"uid":"u","enterpriseId":"e","type":"ultimate","idp":"verified"}]}}`), nil
	})
	c.RefreshIdentityCheck = "strict"
	a := &auth.Auth{UID: "u", EnterpriseID: "e", Domain: "www.workbuddy.ai", AccessToken: "old", RefreshToken: "r"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatal(err)
	}
	if a.Snapshot().AccountType != "ultimate" || a.Snapshot().IDP != "verified" {
		t.Fatal("metadata not enrolled")
	}
}

func TestRefreshFailureCanRetryAndRegionCannotChange(t *testing.T) {
	var count atomic.Int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if count.Add(1) == 1 {
			return jsonResp(503, `{}`), nil
		}
		return jsonResp(200, `{"code":0,"data":{"accessToken":"new","domain":"www.workbuddy.ai"}}`), nil
	})
	c.RefreshIdentityCheck = "best_effort"
	a := &auth.Auth{Domain: "copilot.tencent.com", AccessToken: "old", RefreshToken: "r"}
	if c.RefreshToken(a) == nil || c.RefreshToken(a) == nil {
		t.Fatal("failure/region switch accepted")
	}
	if count.Load() != 2 || a.Snapshot().AccessToken != "old" {
		t.Fatal("failed flight cached or token changed")
	}
}
