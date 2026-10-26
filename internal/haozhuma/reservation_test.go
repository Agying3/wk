package haozhuma

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSMSConcurrentDuplicateHasSingleOwner(t *testing.T) {
	var cleanups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "getPhone":
			fmt.Fprint(w, `{"code":"0","phone":"17000000001"}`)
		case "getMessage":
			fmt.Fprint(w, `{"code":"0","sms":"验证码 123456"}`)
		default:
			cleanups.Add(1)
			fmt.Fprint(w, `{"code":"0"}`)
		}
	}))
	defer srv.Close()
	c := New("fixture")
	c.Base = srv.URL
	var accepted, rejected atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.GetPhone(t.Context(), "52283")
			if err == nil {
				accepted.Add(1)
			} else if errors.Is(err, ErrDuplicatePhone) {
				rejected.Add(1)
			} else {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 || rejected.Load() != 7 || cleanups.Load() != 0 {
		t.Fatalf("owners=%d rejected=%d cleanup_calls=%d", accepted.Load(), rejected.Load(), cleanups.Load())
	}
	if _, err := c.GetMessage(t.Context(), "wrong-project", "17000000001"); err != nil {
		t.Fatal("original owner lost its route", err)
	}
	c.ForgetPhone("17000000001")
	if _, err := c.GetPhone(context.Background(), "52283"); err != nil {
		t.Fatal("completed reservation was not reusable", err)
	}
}

func TestSMSCrossSupplierDuplicateDoesNotReleaseOwner(t *testing.T) {
	var haoMessages, unexpected atomic.Int32
	hao := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("api") {
		case "getPhone":
			fmt.Fprint(w, `{"code":"0","phone":"17000000001"}`)
		case "getMessage":
			haoMessages.Add(1)
			fmt.Fprint(w, `{"code":"0","sms":"验证码 123456"}`)
		default:
			unexpected.Add(1)
		}
	}))
	defer hao.Close()
	ec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("code") != "getPhone" {
			unexpected.Add(1)
		}
		fmt.Fprint(w, "17000000001")
	}))
	defer ec.Close()
	c := New("hao-fixture")
	c.Base = hao.URL
	fallback := NewEcode(ec.URL, "ec-fixture")
	fallback.Keyword = "腾讯科技"
	if err := c.SetFallback(fallback); err != nil {
		t.Fatal(err)
	}
	phone, err := c.GetPhone(WithMode(t.Context(), "haozhuma"), "52283")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.GetPhone(WithMode(t.Context(), "ecode"), "52283"); !errors.Is(err, ErrDuplicatePhone) {
		t.Fatalf("duplicate supplier accepted: %v", err)
	}
	if _, err = c.GetMessage(t.Context(), "52283", phone); err != nil || haoMessages.Load() != 1 || unexpected.Load() != 0 {
		t.Fatal("duplicate altered or released original supplier route")
	}
}
