package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSMSManualCardTypeReachesSupplierAndResets(t *testing.T) {
	h, f, path := newWebuiTestHandler(t, `{"sms":{"haozhuma":{"sid":"52283","isp":"5"}},"autoenroll":{"watch":{"workers":3}}}`)
	sms := successSMSManager(t)
	h.cfg.SMSLogin = sms
	en := h.cfg.AutoEnroll
	en.sms = sms
	en.persist = func(accountCredential, string) (map[string]any, int, error) { return nil, 200, nil }
	f.uniquePhones.Store(true)
	f.msgResp.Store(`{"code":"0","sms":"验证码123456"}`)
	f.phoneQueries = make(chan url.Values, 8)
	client := en.CurrentSMSClient()
	client.ISP = "5"
	client.SetUID("52283-MANUAL")
	watch := NewUIDWatcher(WatchConfig{Workers: 3, Projects: []WatchProject{{Sid: "52283", CardType: "2"}}}, nil, en, "")
	h.cfg.UIDWatcher = watch
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, card := range []string{"2", "1", ""} {
		w := httptest.NewRecorder()
		body := fmt.Sprintf(`{"count":1,"workers":1,"poll_count":1,"max_attempts":1,"provider":"haozhuma","haozhuma_card_type":%q}`, card)
		h.ServeHTTP(w, httptest.NewRequest("POST", "/admin/account/sms/auto-enroll", strings.NewReader(body)))
		if w.Code != http.StatusAccepted {
			t.Fatalf("card %q: %d %s", card, w.Code, w.Body.String())
		}
		waitDone(t, en)
		if en.Status().OK != 1 {
			t.Fatalf("fixture enrollment failed: %+v", en.Status())
		}
		select {
		case q := <-f.phoneQueries:
			if q.Get("ascription") != card || q.Get("isp") != "5" || q.Get("uid") != "52283-MANUAL" {
				t.Fatalf("manual selection or saved fetch options lost: %v", q)
			}
		default:
			t.Fatal("no supplier getPhone request")
		}
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/admin/account/sms/auto-enroll", nil))
		var st struct {
			CardType string `json:"haozhuma_card_type"`
			Source   string `json:"source"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || st.CardType != card || st.Source != "manual" {
			t.Fatalf("task selection not reported or reset: %s %v", w.Body.String(), err)
		}
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || watch.cfg.Projects[0].CardType != "2" || watch.cfg.Workers != 3 || client.ISP != "5" || client.UID() != "52283-MANUAL" {
		t.Fatal("per-task card choice modified saved manual settings or monitor projects")
	}
}

func TestSMSManualInvalidCardTypeDoesNotContactSupplier(t *testing.T) {
	h, _, _ := newWebuiTestHandler(t, `{}`)
	h.cfg.SMSLogin = noSMSManager()
	var calls atomic.Int32
	h.cfg.AutoEnroll.CurrentSMSClient().HTTP = &http.Client{Transport: monitorRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return http.DefaultTransport.RoundTrip(r)
	})}
	for _, item := range []struct{ provider, card string }{{"haozhuma", "0"}, {"haozhuma", "3"}, {"haozhuma", "physical"}, {"ecode", "2"}} {
		w := httptest.NewRecorder()
		body := fmt.Sprintf(`{"count":1,"provider":%q,"haozhuma_card_type":%q,"use_saved_platforms":true}`, item.provider, item.card)
		h.ServeHTTP(w, httptest.NewRequest("POST", "/admin/account/sms/auto-enroll", strings.NewReader(body)))
		if w.Code != 400 || !strings.Contains(w.Body.String(), "号码类型") {
			t.Fatalf("accepted invalid or wrong-provider selection: %d %s", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 0 || h.cfg.AutoEnroll.Status().Running {
		t.Fatal("invalid selection started work or contacted supplier")
	}
}
