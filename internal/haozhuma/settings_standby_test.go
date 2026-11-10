package haozhuma

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBuildStandbyDefersSupplierLoginUntilActivation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("api") != "login" {
			t.Errorf("unexpected supplier operation")
		}
		_, _ = w.Write([]byte(`{"code":"0","msg":"ok","token":"activated-test-token"}`))
	}))
	defer srv.Close()
	c, err := BuildStandby(Settings{User: "test-user", Pass: "test-password", Sid: "123", Fallback: "test-ecode-token", EcodeKeyword: "test-keyword"}, srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || c == nil || c.Fallback() == nil || c.Project() != "123" {
		t.Fatal("standby contacted supplier or lost configuration")
	}
	active, err := c.ActivateStandby(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || active.tokenValue() != "activated-test-token" || active.Project() != "123" || active.Fallback() == nil {
		t.Fatal("activation did not retain supplier configuration")
	}
	again, err := active.ActivateStandby(context.Background())
	if err != nil || again != active || calls.Load() != 1 {
		t.Fatal("active supplier logged in twice")
	}
}

func TestBuildStandbyPreservesTokenOnlyAndEcodeModes(t *testing.T) {
	for _, settings := range []Settings{
		{Token: "test-token", Sid: "123"},
		{Provider: "ecode", Token: "test-ecode-token", EcodeKeyword: "test-keyword", Mode: "ecode"},
	} {
		c, err := BuildStandby(settings, "")
		if err != nil {
			t.Fatal(err)
		}
		active, err := c.ActivateStandby(context.Background())
		if err != nil || active == nil || active.Project() != c.Project() || active.tokenValue() != c.tokenValue() {
			t.Fatal("token-only activation changed supplier configuration")
		}
	}
}
