package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/haozhuma"
)

func TestSMSRejectedFallbackDoesNotPersist(t *testing.T) {
	var calls atomic.Int32
	ec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(401)
	}))
	defer ec.Close()
	seed := map[string]any{"sms": map[string]any{"haozhuma": map[string]any{
		"token": "fixture-hz", "sid": "52283", "ecode_keyword": "腾讯科技", "fallback": "old-fixture", "fallback_base": ec.URL,
	}}}
	raw, _ := json.Marshal(seed)
	h, _, path := newWebuiTestHandler(t, string(raw))
	before, _ := os.ReadFile(path)
	r := httptest.NewRequest("POST", "/admin/config", strings.NewReader(`{"sms":{"haozhuma":{"fallback":"invalid-fixture"}}}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || calls.Load() != 1 {
		t.Fatalf("expected actual failed verification, code=%d calls=%d: %s", w.Code, calls.Load(), w.Body.String())
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("rejected credential was persisted")
	}
}

func TestSMSUpdateCommitGuardsAndWriteFailure(t *testing.T) {
	original := haozhuma.New("original-fixture")
	replacement := haozhuma.New("replacement-fixture")
	a := NewAutoEnroller(nil, original, "52283", nil, nil, nil)
	a.running = true
	written := false
	err := a.ApplySMSUpdate(replacement, nil, func() error { written = true; return nil })
	if !errors.Is(err, ErrBusyAuth) || written {
		t.Fatal("active task allowed credential update")
	}
	a.running = false
	mode := "haozhuma"
	err = a.ApplySMSUpdate(nil, &mode, func() error { return errors.New("fixture disk error") })
	if err == nil || original.Mode() != "auto" {
		t.Fatal("failed persistence changed runtime mode")
	}
	mode = "ecode"
	err = a.ApplySMSUpdate(nil, &mode, func() error { written = true; return nil })
	if err == nil || written {
		t.Fatal("unconfigured supplier was persisted")
	}
}

func TestSMSTaskSelectionDoesNotChangeNextTask(t *testing.T) {
	hz := newFakeHZM(t)
	hz.summary.Store(`{"code":"0","money":"20","num":0}`)
	var ecCalls atomic.Int32
	ec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("code") {
		case "leftAmount":
			fmt.Fprint(w, "20.00")
		case "getPhone":
			ecCalls.Add(1)
			fmt.Fprint(w, "17100000001")
		case "getMsg":
			fmt.Fprint(w, "【腾讯科技】验证码 123456")
		}
	}))
	defer ec.Close()
	c := hz.client()
	fb := haozhuma.NewEcode(ec.URL, "ec-fixture")
	fb.Keyword = "腾讯科技"
	if err := c.SetFallback(fb); err != nil {
		t.Fatal(err)
	}
	if err := c.SetMode("haozhuma"); err != nil {
		t.Fatal(err)
	}
	a := NewAutoEnroller(successSMSManager(t), c, "52283", func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil }, nil, nil)
	a.pollInterval = time.Millisecond
	if err := a.AutoRunWith(AutoRunOptions{Want: 1, Workers: 1, ProviderMode: "ecode"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop("test complete") })
	waitFor(t, 5*time.Second, func() bool { return !a.Status().Running }, "task completion")
	if a.Status().OK != 1 || ecCalls.Load() != 1 || hz.getPhone.Load() != 0 {
		t.Fatal("task did not use Ecode")
	}
	if c.Mode() != "haozhuma" {
		t.Fatal("task override leaked into global mode")
	}
	_, err := c.GetPhone(context.Background(), "52283")
	if err != nil || hz.getPhone.Load() != 1 {
		t.Fatalf("next task used wrong provider: %v", err)
	}
}

func TestSMSModePatchPreservesSchedule(t *testing.T) {
	seed := `{"schedule":{"checkin_hours":[9],"keepalive_hours":[22]},"sms":{"haozhuma":{"token":"fixture","sid":"52283"}}}`
	h, _, path := newWebuiTestHandler(t, seed)
	out := doAdminConfig(t, h, "POST", map[string]any{"sms": map[string]any{"haozhuma": map[string]any{"mode": "haozhuma"}}})
	if out["ok"] != true {
		t.Fatal(out)
	}
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	schedule := doc["schedule"].(map[string]any)
	if len(schedule["checkin_hours"].([]any)) != 1 || len(schedule["keepalive_hours"].([]any)) != 1 {
		t.Fatal("partial patch cleared schedule")
	}
}
