package haozhuma

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func smsFixture(t *testing.T, down bool) (*Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	hzCalls, ecCalls := &atomic.Int32{}, &atomic.Int32{}
	hz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hzCalls.Add(1)
		if down {
			w.WriteHeader(466)
			return
		}
		fmt.Fprintf(w, `{"code":"0","phone":"13900000001","money":"20","num":0,"sms":"验证码 123456"}`)
	}))
	ec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := ecCalls.Add(1)
		q := r.URL.Query()
		switch q.Get("code") {
		case "leftAmount":
			fmt.Fprint(w, "10.50")
		case "getPhone":
			if q.Get("keyWord") != "腾讯科技" {
				t.Errorf("wrong keyword: %q", q.Get("keyWord"))
			}
			fmt.Fprintf(w, "171%08d", n)
		case "getMsg":
			if q.Get("keyWord") != "腾讯科技" {
				t.Errorf("wrong message keyword")
			}
			fmt.Fprint(w, "【腾讯科技】验证码 654321")
		}
	}))
	t.Cleanup(hz.Close)
	t.Cleanup(ec.Close)
	c := New("fixture-hz")
	c.Base = hz.URL
	fb := NewEcode(ec.URL, "fixture-ec")
	fb.Keyword = "腾讯科技"
	if err := c.SetFallback(fb); err != nil {
		t.Fatal(err)
	}
	return c, hzCalls, ecCalls
}

func TestSMSFallbackPreservesKeywordAndCleanup(t *testing.T) {
	c, hz, _ := smsFixture(t, true)
	p, err := c.GetPhone(context.Background(), "52283")
	if err != nil {
		t.Fatal(err)
	}
	before := hz.Load()
	if err := c.SetMode("haozhuma"); err != nil {
		t.Fatal(err)
	}
	msg, err := c.GetMessage(context.Background(), "different-project", p)
	if err != nil || !strings.Contains(msg, "654321") {
		t.Fatalf("message lost its supplier: %q %v", msg, err)
	}
	if err := c.Blacklist(context.Background(), "52283", p); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(context.Background(), "52283", p); err != nil {
		t.Fatal(err)
	}
	if hz.Load() != before {
		t.Fatal("Ecode cleanup reached Haozhuma")
	}
	c.ForgetPhone(p)
}

func TestSMSModesAndTemporarySelection(t *testing.T) {
	c, hz, ec := smsFixture(t, false)
	ctx := WithMode(context.Background(), "ecode")
	p, err := c.GetPhone(ctx, "52283")
	if err != nil || hz.Load() != 0 || ec.Load() != 1 {
		t.Fatalf("temporary Ecode selection: %v", err)
	}
	c.ForgetPhone(p)
	if c.Mode() != "auto" {
		t.Fatal("task override changed global mode")
	}
	_, err = c.GetPhone(context.Background(), "52283")
	if err != nil || hz.Load() != 1 {
		t.Fatal("next task did not return to primary")
	}
	c.ForgetPhone("13900000001")
	if err := c.SetMode("both"); err != nil {
		t.Fatal(err)
	}
	h, e := hz.Load(), ec.Load()
	for i := 0; i < 2; i++ {
		phone, err := c.GetPhone(context.Background(), "52283")
		if err != nil {
			t.Fatal(err)
		}
		c.ForgetPhone(phone)
	}
	if hz.Load() != h+1 || ec.Load() != e+1 {
		t.Fatal("both mode did not alternate")
	}
	primary := New("fixture")
	if err := primary.SetMode("ecode"); err == nil {
		t.Fatal("missing Ecode must be rejected")
	}
	if _, err := primary.GetPhone(WithMode(context.Background(), "ecode"), "52283"); err == nil {
		t.Fatal("missing Ecode silently used primary")
	}
}

func TestSMSActiveRoutesAreNotEvicted(t *testing.T) {
	c, _, _ := smsFixture(t, true)
	ctx := WithMode(context.Background(), "ecode")
	var first string
	for i := 0; i < 300; i++ {
		p, err := c.GetPhone(ctx, "52283")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = p
		}
	}
	if _, err := c.GetMessage(ctx, "52283", first); err != nil {
		t.Fatal(err)
	}
	if !c.IsEcodePhone(first) {
		t.Fatal("active supplier route was evicted")
	}
}

func TestSMSConcurrentModeChanges(t *testing.T) {
	c, _, _ := smsFixture(t, false)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			for _, m := range []string{"auto", "haozhuma", "ecode", "both"} {
				if err := c.SetMode(m); err != nil {
					t.Error(err)
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		ctx := WithMode(context.Background(), "ecode")
		for i := 0; i < 40; i++ {
			p, err := c.GetPhone(ctx, "52283")
			if err != nil {
				t.Error(err)
				return
			}
			_, err = c.GetMessage(ctx, "52283", p)
			if err != nil {
				t.Error(err)
			}
			_ = c.Blacklist(ctx, "52283", p)
			_ = c.Release(ctx, "52283", p)
			c.ForgetPhone(p)
		}
	}()
	wg.Wait()
}

func TestSMSSummaryPartialFailureRetainsBalance(t *testing.T) {
	c, _, _ := smsFixture(t, true)
	s, err := c.Summary(context.Background())
	if err != nil || s.Balance != 10.5 || s.Warning == "" {
		t.Fatalf("usable balance was lost: %+v %v", s, err)
	}
}

func TestEcodeWaitAndSecretRedaction(t *testing.T) {
	action := "[尚未收到]"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, action) }))
	defer srv.Close()
	c := NewEcode(srv.URL, "fixture-secret")
	c.Keyword = "腾讯科技"
	msg, err := c.GetMessage(context.Background(), "52283", "17100000001")
	if err != nil || msg != "" {
		t.Fatalf("waiting response: %q %v", msg, err)
	}
	action = "ERROR:fixture-secret invalid"
	_, err = c.GetMessage(context.Background(), "52283", "17100000001")
	if err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("secret not redacted: %v", err)
	}
}

func TestSMSSettingsValidation(t *testing.T) {
	for _, s := range []Settings{
		{Provider: "ecode", Token: "fake", Sid: "52283"},
		{Token: "fake", Fallback: "fake"},
		{Token: "fake", EcodeBase: "https://user:pass@example.org/path"},
		{Token: "fake", FallbackCardType: "invalid"},
	} {
		if err := s.Validate(); err == nil {
			t.Fatalf("invalid settings accepted: %+v", s)
		}
	}
	c, _, _ := smsFixture(t, false)
	if _, err := c.GetPhone(WithMode(context.Background(), "unknown"), "52283"); err == nil {
		t.Fatal("unknown mode accepted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GetPhone(cancelled, "52283"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
