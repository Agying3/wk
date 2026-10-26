package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/haozhuma"
	"workbuddy2api/internal/smslogin"
)

func TestSMSRepeatedEcodeNumbersAreSentOncePerBatch(t *testing.T) {
	var phones atomic.Int32
	ec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("code") {
		case "leftAmount":
			fmt.Fprint(w, "20.00")
		case "getPhone":
			fmt.Fprintf(w, "1700000000%d", phones.Add(1)%2+1)
		default:
			t.Error("failed sends must not poll or clean up at Ecode")
		}
	}))
	defer ec.Close()
	up := newFakeSMSUpstream(t)
	defer up.Close()
	var mu sync.Mutex
	sends := map[string]int{}
	up.oneID.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Mobile string `json:"mobile"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sends[body.Mobile]++
		mu.Unlock()
		smsRaw(w, 400, `{"errCode":"E0010343","errMessage":"fixture rejection"}`)
	})
	c := haozhuma.NewEcode(ec.URL, "fixture")
	c.Keyword = "腾讯科技"
	a := NewAutoEnroller(smslogin.NewManager(up.Endpoints(), time.Minute), c, "腾讯科技", nil, nil, nil)
	a.retryDelay = time.Millisecond
	a.SetLimits(0, 100)
	t.Cleanup(func() { a.Stop("test complete"); waitDone(t, a) })
	if err := a.AutoRunWith(AutoRunOptions{Want: 8, Workers: 8, MaxAttempts: 24}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, a)
	mu.Lock()
	if len(sends) != 2 {
		t.Errorf("unique SMS destinations=%d want 2", len(sends))
	}
	for _, count := range sends {
		if count != 1 {
			t.Errorf("duplicate SMS send count=%d want 1", count)
		}
	}
	mu.Unlock()
	st := a.Status()
	if st.Consumed != 2 || st.Held != 0 || st.Fail != 2 || !strings.Contains(st.StopReason, "重复") {
		t.Fatalf("duplicates corrupted task accounting: %+v", st)
	}
	if strings.Contains(strings.Join(st.Logs, "\n"), "已拉黑") {
		t.Fatal("Ecode falsely reported platform blacklisting")
	}
	if err := a.AutoRunWith(AutoRunOptions{Want: 1, Workers: 1, MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, a)
	if a.Status().Consumed != 1 {
		t.Fatal("new batch did not reset Ecode attempt history")
	}
}

func TestSMSEcodePollRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		status, failures, budget, wantCalls int
		wantSuccess                         bool
	}{
		{"recover_500", 500, 2, 8, 3, true},
		{"recover_429", 429, 1, 8, 2, true},
		{"bounded_503", 503, 100, 18, 4, false},
		{"poll_budget", 500, 100, 2, 2, false},
		{"invalid_token", 401, 100, 18, 1, false},
		{"bad_request", 400, 100, 18, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if int(calls.Add(1)) <= tc.failures {
					w.WriteHeader(tc.status)
					return
				}
				fmt.Fprint(w, "【腾讯科技】验证码 654321")
			}))
			defer srv.Close()
			c := haozhuma.NewEcode(srv.URL, "fixture-secret")
			c.Keyword = "腾讯科技"
			a := NewAutoEnroller(nil, c, "腾讯科技", nil, nil, nil)
			a.pollInterval = time.Millisecond
			code, err := a.pollCode(t.Context(), "腾讯科技", "17000000001", tc.budget)
			if int(calls.Load()) != tc.wantCalls || (err == nil) != tc.wantSuccess {
				t.Fatalf("calls=%d code=%q err=%v", calls.Load(), code, err)
			}
			if tc.wantSuccess && code != "654321" {
				t.Fatalf("recovery lost verification code: %q", code)
			}
			if err != nil && strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal("retry error exposed token")
			}
		})
	}
}

func TestSMSEcodePollRecoversFromRequestTimeout(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "验证码 654321")
	}))
	defer srv.Close()
	c := haozhuma.NewEcode(srv.URL, "fixture")
	c.Timeout = 20 * time.Millisecond
	a := NewAutoEnroller(nil, c, "腾讯科技", nil, nil, nil)
	a.pollInterval = time.Millisecond
	code, err := a.pollCode(t.Context(), "腾讯科技", "17000000001", 3)
	if err != nil || code != "654321" || calls.Load() != 2 {
		t.Fatalf("timeout not recovered: calls=%d code=%q err=%v", calls.Load(), code, err)
	}
}

func TestSMSEcodeRetryBackoffCanBeStopped(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := haozhuma.NewEcode(srv.URL, "fixture")
	a := NewAutoEnroller(nil, c, "腾讯科技", nil, nil, nil)
	a.pollInterval = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.pollCode(ctx, "腾讯科技", "17000000001", 18); done <- err }()
	waitFor(t, time.Second, func() bool { return len(a.Status().Logs) > 0 }, "retry backoff")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("cancel retried or lost cancellation: calls=%d err=%v", calls.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not interrupt retry backoff")
	}
}

func TestSMSEcodeFailureLogDoesNotClaimBlacklisting(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("code") {
		case "getPhone":
			fmt.Fprint(w, "17000000001")
		case "getMsg":
			calls.Add(1)
			w.WriteHeader(500)
		default:
			t.Error("unexpected Ecode cleanup")
		}
	}))
	defer srv.Close()
	c := haozhuma.NewEcode(srv.URL, "fixture")
	c.Keyword = "腾讯科技"
	a := NewAutoEnroller(successSMSManager(t), c, "腾讯科技", nil, nil, nil)
	a.pollInterval = time.Millisecond
	_, used, err := a.tryOne(t.Context(), 0, 8)
	logs := strings.Join(a.Status().Logs, "\n")
	if err == nil || !used || calls.Load() != 4 || a.Status().Held != 0 || !strings.Contains(logs, "收码未完成") ||
		!strings.Contains(logs, "未调用平台拉黑或释放") || strings.Contains(logs, "已拉黑") {
		t.Fatalf("misleading cleanup: calls=%d err=%v logs=%s", calls.Load(), err, logs)
	}
}
