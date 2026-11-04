package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReceivedSMSBlacklistsWithoutRelease(t *testing.T) {
	for _, tc := range []struct {
		name        string
		persistFail bool
		blackFails  int32
	}{
		{name: "success"},
		{name: "local_persist_failure", persistFail: true},
		{name: "blacklist_retry", blackFails: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHZM(t)
			f.msgResp.Store(`{"code":"0","msg":"ok","sms":"验证码123456"}`)
			f.blacklistFail.Store(tc.blackFails)
			var en *AutoEnroller
			en = NewAutoEnroller(successSMSManager(t), f.client(), "52283",
				func(accountCredential, string) (map[string]any, int, error) {
					// 拉黑在验码/账号落盘之前就已发起。
					if f.blacklistCalls.Load() != 1 {
						t.Error("blacklisting was not attempted immediately after receiving SMS")
					}
					if en.Status().Held != 1 {
						t.Error("local phone lease ended before login finished")
					}
					if tc.persistFail {
						return nil, 500, errors.New("fixture disk error")
					}
					return nil, 200, nil
				}, nil, nil)
			en.SetLedgerPath(filepath.Join(t.TempDir(), "held.json"))
			en.pollInterval = time.Millisecond
			if err := en.AutoRunWith(AutoRunOptions{Want: 1, Workers: 1, MaxAttempts: 1}); err != nil {
				t.Fatal(err)
			}
			waitDone(t, en)
			st := en.Status()
			if f.blacklist.Load() != 1 || f.blacklistCalls.Load() != 1+tc.blackFails || f.firstReleaseSeq.Load() != 0 {
				t.Fatalf("blacklist success/calls=%d/%d releaseSeq=%d", f.blacklist.Load(), f.blacklistCalls.Load(), f.firstReleaseSeq.Load())
			}
			if st.Held != 0 || st.Released != 1 || st.Received != 1 {
				t.Fatalf("wrong cleanup/accounting: %+v", st)
			}
		})
	}
}

func TestReceivedSMSBlacklistSurvivesRestart(t *testing.T) {
	f := newFakeHZM(t)
	f.msgResp.Store(`{"code":"0","msg":"ok","sms":"验证码123456"}`)
	f.blacklistFail.Store(100)
	ledger := filepath.Join(t.TempDir(), "held.json")
	en := NewAutoEnroller(successSMSManager(t), f.client(), "61903",
		func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil }, nil, nil)
	en.SetLedgerPath(ledger)
	if err := en.AutoRunWith(AutoRunOptions{Want: 1, Workers: 1, MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, en)
	raw, err := os.ReadFile(ledger)
	var saved heldLedger
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Version != 2 || len(saved.Phones) != 1 || saved.Blacklists[saved.Phones[0]] != "61903" {
		t.Fatalf("pending blacklist lost: %s %v", raw, err)
	}
	if strings.Contains(string(raw), "123456") || strings.Contains(string(raw), "tok-1") {
		t.Fatal("ledger contains SMS or credentials")
	}
	if en.Status().Held != 1 || f.firstReleaseSeq.Load() != 0 || f.blacklistCalls.Load() != 2 {
		t.Fatal("teardown did not retry blacklisting exclusively")
	}
	// 模拟重启，当前项目已变化，且供应商称号码不存在：不能把它当作已拉黑。
	client := f.client()
	client.HTTP = &http.Client{Transport: monitorRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("api") == "addBlacklist" && r.URL.Query().Get("sid") != "61903" {
			t.Error("pending blacklist lost original project after restart")
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	restarted := NewAutoEnroller(nil, client, "52283", nil, nil, nil)
	restarted.SetLedgerPath(ledger)
	f.blacklistGone.Store(true)
	if n := restarted.ReclaimOrphans(); n != 0 || restarted.Status().Held != 1 {
		t.Fatal("missing number was incorrectly treated as confirmed blacklisting")
	}
	f.blacklistGone.Store(false)
	f.blacklistFail.Store(0)
	if n := restarted.ReclaimOrphans(); n != 1 || restarted.Status().Held != 0 || f.firstReleaseSeq.Load() != 0 {
		t.Fatal("restart did not complete original blacklist without release")
	}
	raw, _ = os.ReadFile(ledger)
	if string(raw) != "[]" {
		t.Fatalf("completed blacklist remains in ledger: %s", raw)
	}
}

func TestReleaseAllPreservesPendingBlacklist(t *testing.T) {
	f := newFakeHZM(t)
	client := f.client()
	client.HTTP = &http.Client{Transport: monitorRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("api") == "cancelAllRecv" {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":"0","msg":"ok"}`))}, nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	en := NewAutoEnroller(nil, client, "52283", nil, nil, nil)
	en.SetLedgerPath(filepath.Join(t.TempDir(), "held.json"))
	en.trackHeld("17000000001")
	en.trackHeld("17000000002")
	en.trackBlacklist("17000000001", "61903")
	if done, before, err := en.ReleaseAllHeld(context.Background()); !done || before != 2 || err != nil {
		t.Fatalf("release all: %v %d %v", done, before, err)
	}
	if en.Status().Held != 1 || en.Status().Released != 1 {
		t.Fatal("release all discarded pending blacklist")
	}
	if n := en.reclaimStuck(); n != 1 || f.blacklist.Load() != 1 || f.firstReleaseSeq.Load() != 0 {
		t.Fatal("pending blacklist was downgraded after release all")
	}
}
