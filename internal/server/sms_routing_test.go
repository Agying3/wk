package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/haozhuma"
	"workbuddy2api/internal/haozhumah5"
)

func TestSMSColdStartCanEnableEcodeWithoutHaozhuma(t *testing.T) {
	var haoCalls, ecCalls atomic.Int32
	hao := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		haoCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer hao.Close()
	oldBase := haozhumaBase
	haozhumaBase = hao.URL
	t.Cleanup(func() { haozhumaBase = oldBase })
	ec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ecCalls.Add(1)
		if r.URL.Query().Get("token") == "rejected-fixture" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Query().Get("code") {
		case "leftAmount":
			fmt.Fprint(w, "20.00")
		case "getPhone":
			if r.URL.Query().Get("keyWord") != "腾讯科技" {
				t.Error("legacy numeric sid used for Ecode")
			}
			fmt.Fprint(w, "17100000001")
		case "getMsg":
			fmt.Fprint(w, "【腾讯科技】验证码 123456")
		default:
			t.Error("unexpected Ecode action")
		}
	}))
	defer ec.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	seed := `{"sms":{"haozhuma":{"user":"bad-fixture","pass":"bad-fixture","sid":"52283"}}}`
	if err := os.WriteFile(path, []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, Config{ConfigPath: path, SMSLogin: successSMSManager(t)})
	if h.cfg.AutoEnroll == nil {
		t.Fatal("cold start must allow supplier configuration")
	}
	status := func() map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/admin/account/sms/auto-enroll", nil))
		if w.Code != 200 {
			t.Fatalf("status unavailable: %d %s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if status()["enabled"] != false || ecCalls.Load() != 0 {
		t.Fatal("unconfigured status must be readable without supplier calls")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/admin/account/sms/auto-enroll", strings.NewReader(`{"count":1}`)))
	if w.Code != 400 {
		t.Fatalf("unconfigured task must be rejected: %d", w.Code)
	}
	if _, _, err := h.cfg.AutoEnroll.ReleaseAllHeld(t.Context()); err == nil {
		t.Fatal("release without a supplier must fail safely")
	}
	patch := func(token string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"sms": map[string]any{"haozhuma": map[string]any{
			"provider": "ecode", "token": token, "ecode_keyword": "腾讯科技", "ecode_base": ec.URL,
		}}})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/config", strings.NewReader(string(body))))
		return rec
	}
	if rec := patch("rejected-fixture"); rec.Code != 400 {
		t.Fatalf("invalid token accepted: %d", rec.Code)
	}
	if raw, _ := os.ReadFile(path); string(raw) != seed || h.cfg.AutoEnroll.SMSConfigured() {
		t.Fatal("failed verification changed disk or runtime")
	}
	if rec := patch("ecode-fixture"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"haozhuma_auth":"reconnected"`) {
		t.Fatalf("cold-start setup did not activate immediately: %d %s", rec.Code, rec.Body.String())
	}
	checkedCalls := ecCalls.Load()
	if status()["enabled"] != true || h.cfg.AutoEnroll.Sid() != "腾讯科技" || ecCalls.Load() != checkedCalls {
		t.Fatal("Ecode runtime not activated with independent keyword")
	}
	// Exercise the actual start route against local SMS/Ecode fakes.
	a := h.cfg.AutoEnroll
	a.persist = func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil }
	a.pollInterval = time.Millisecond
	t.Cleanup(func() { a.Stop("test complete"); waitDone(t, a) })
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/admin/account/sms/auto-enroll", strings.NewReader(`{"count":1,"workers":1,"provider":"ecode"}`)))
	if w.Code != 202 {
		t.Fatalf("Ecode task rejected after save: %d %s", w.Code, w.Body.String())
	}
	waitFor(t, 5*time.Second, func() bool { return !a.Status().Running }, "cold-start Ecode task")
	if a.Status().OK != 1 || haoCalls.Load() != 0 {
		t.Fatalf("Ecode task failed or contacted Haozhuma: success=%d hao_calls=%d", a.Status().OK, haoCalls.Load())
	}
}

func TestSMSColdStartPreservesHaozhumaOrphanLedger(t *testing.T) {
	for _, client := range []*haozhuma.Client{nil, haozhuma.NewEcode("http://127.0.0.1:1", "fixture")} {
		path := filepath.Join(t.TempDir(), "orphans.json")
		original := `["13900000001"]`
		if err := os.WriteFile(path, []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
		a := NewAutoEnroller(nil, client, "腾讯科技", nil, nil, nil)
		a.SetLedgerPath(path)
		if a.ReclaimOrphans() != 0 {
			t.Fatal("unconfigured/Ecode client reclaimed Haozhuma numbers")
		}
		a.mu.Lock()
		a.persistLedgerLocked()
		a.mu.Unlock()
		if raw, _ := os.ReadFile(path); string(raw) != original {
			t.Fatal("unconfigured/Ecode client erased Haozhuma orphan ledger")
		}
	}
}

func TestSMSWatcherPausesWithoutHaozhuma(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
	}))
	defer srv.Close()
	oldBase := haozhumah5.BaseURL
	haozhumah5.BaseURL = srv.URL
	t.Cleanup(func() { haozhumah5.BaseURL = oldBase })
	for _, client := range []*haozhuma.Client{nil, haozhuma.NewEcode(srv.URL, "fixture")} {
		a := NewAutoEnroller(nil, client, "腾讯科技", nil, nil, nil)
		w := NewUIDWatcher(WatchConfig{}, haozhumah5.New("fixture-session"), a, "")
		w.tick(t.Context(), WatchConfig{})
		if w.Status().Available || calls.Load() != 0 {
			t.Fatal("watcher contacted Haozhuma without a configured Haozhuma supplier")
		}
	}
}

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
