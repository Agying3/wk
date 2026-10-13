package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/groups"
	"workbuddy2api/internal/traffic"
)

func trafficRow(t *testing.T, s traffic.Snapshot, dim, id string) traffic.Row {
	t.Helper()
	for _, r := range s.Rows {
		if r.Dimension == dim && r.ID == id {
			return r
		}
	}
	t.Fatalf("missing traffic row %s/%s", dim, id)
	return traffic.Row{}
}

func TestTrafficRetriesKeysMembershipUsageAndPrivacy(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			g, err := groups.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := g.Create("vip"); err != nil {
				t.Fatal(err)
			}
			for _, uid := range []string{"bad", "good"} {
				if err := g.SetAccountGroups(uid, []string{"vip", "default"}); err != nil {
					t.Fatal(err)
				}
			}
			key, err := g.CreateKey("customer", "vip")
			if err != nil {
				t.Fatal(err)
			}
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				if authz == "Bearer at-bad" {
					return 402, `{"code":1,"msg":"余额不足"}`, false
				}
				return 200, sseOK, true
			})
			p := testPoolWith(&auth.Auth{UID: "bad", Nickname: "13800138000", AccessToken: "at-bad", ExpiresAt: 9999999999}, &auth.Auth{UID: "good", Nickname: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
			p.SetCredits("bad", 2000)
			h := newTestHandler(t, Config{APIKey: "fixture-admin", Pool: p, Groups: g, Upstream: up, CreditPolicy: CreditPolicy{InputPer1K: 1, OutputPer1K: 1}})
			body := `{"model":"glm-5.2","messages":[],"stream":true}`
			if path == "/v1/responses" {
				body = `{"model":"glm-5.2","input":"hello","stream":true}`
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+key.Key)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("response=%d", rec.Code)
			}
			s, err := h.cfg.Traffic.Snapshot(context.Background(), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			total := trafficRow(t, s, "total", "all")
			if total.Requests != 1 || total.Successes != 1 || total.Failures != 0 || total.CurrentConcurrency != 0 || total.InputTokens != 1 || total.OutputTokens != 1 || total.CreditsEstimated <= 0 {
				t.Fatalf("client stats: %+v", total)
			}
			keyRow := trafficRow(t, s, "key", key.ID)
			if keyRow.Requests != 1 || keyRow.Name != "customer" {
				t.Fatalf("key stats: %+v", keyRow)
			}
			bad := trafficRow(t, s, "account", "bad")
			good := trafficRow(t, s, "account", "good")
			if bad.Requests != 1 || bad.Failures != 1 || bad.CurrentConcurrency != 0 || bad.Name == "13800138000" || good.Successes != 1 || good.InputTokens != 1 || good.CurrentConcurrency != 0 {
				t.Fatalf("account attempts: bad=%+v good=%+v", bad, good)
			}
			for _, group := range []string{"vip", "default"} {
				r := trafficRow(t, s, "account_group", group)
				if r.Requests != 2 || r.Successes != 1 || r.Failures != 1 || r.CurrentConcurrency != 0 {
					t.Fatalf("group stats: %+v", r)
				}
			}
			proxy := trafficRow(t, s, "proxy", "direct")
			if proxy.Requests != 2 || proxy.Failures != 1 || proxy.Successes != 1 || proxy.CurrentConcurrency != 0 {
				t.Fatalf("proxy stats: %+v", proxy)
			}
			get := httptest.NewRequest(http.MethodGet, "/admin/traffic?window=1h", nil)
			get.Header.Set("Authorization", "Bearer fixture-admin")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, get)
			if out.Code != 200 {
				t.Fatalf("traffic endpoint=%d %s", out.Code, out.Body)
			}
			for _, secret := range []string{key.Key, "fixture-admin", "at-bad", "at-good", "13800138000"} {
				if strings.Contains(out.Body.String(), secret) {
					t.Fatal("traffic endpoint exposed a credential or full phone")
				}
			}
			get.Header.Set("Authorization", "Bearer "+key.Key)
			out = httptest.NewRecorder()
			h.ServeHTTP(out, get)
			if out.Code != 401 {
				t.Fatalf("group key accessed admin statistics: %d", out.Code)
			}
		})
	}
}

func TestTrafficInFlightStreamUntilResponseCloses(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	started := make(chan struct{})
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
	})
	h := newTestHandler(t, Config{Pool: testPoolWith(&auth.Auth{UID: "a", AccessToken: "fixture", ExpiresAt: 9999999999}), Upstream: up})
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not start")
	}
	s, err := h.cfg.Traffic.Snapshot(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"total", "all"}, {"account", "a"}, {"proxy", "direct"}, {"key", "global-admin"}} {
		r := trafficRow(t, s, pair[0], pair[1])
		if r.CurrentConcurrency != 1 || r.Completed != 0 {
			t.Fatalf("stream released early: %+v", r)
		}
	}
	if _, err := io.WriteString(writer, sseOK); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not finish")
	}
	s, _ = h.cfg.Traffic.Snapshot(context.Background(), time.Hour)
	for _, pair := range [][2]string{{"total", "all"}, {"account", "a"}, {"proxy", "direct"}} {
		r := trafficRow(t, s, pair[0], pair[1])
		if r.CurrentConcurrency != 0 || r.Completed != 1 || r.Successes != 1 {
			t.Fatalf("stream lease leaked: %+v", r)
		}
	}
}

func TestTrafficMalformedInputAndInvalidWindow(t *testing.T) {
	h := newTestHandler(t, Config{})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{`)))
	s, _ := h.cfg.Traffic.Snapshot(context.Background(), time.Hour)
	r := trafficRow(t, s, "total", "all")
	if r.Requests != 1 || r.Failures != 1 || r.CurrentConcurrency != 0 {
		t.Fatalf("invalid request: %+v", r)
	}
	for _, window := range []string{"1m", "5m", "1h", "6h", "24h", "7d", "30d"} {
		out := httptest.NewRecorder()
		h.ServeHTTP(out, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/traffic?window=%s", window), nil))
		if out.Code != 200 {
			t.Fatalf("window %s: %d", window, out.Code)
		}
		var data traffic.Snapshot
		if err := json.Unmarshal(out.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
	}
	out := httptest.NewRecorder()
	h.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/admin/traffic?window=all", nil))
	if out.Code != 400 {
		t.Fatalf("unknown window=%d", out.Code)
	}
}

func TestTrafficCachePresenceForStreamAndSync(t *testing.T) {
	for _, stream := range []bool{true, false} {
		for _, tc := range []struct {
			name, usage string
			known       bool
			rate        float64
		}{
			{"unknown", `"prompt_tokens":100,"completion_tokens":1`, false, 0},
			{"zero", `"prompt_tokens":100,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":0}`, true, 0},
			{"hit", `"prompt_tokens":100,"completion_tokens":1,"cache_read_input_tokens":60`, true, 60},
		} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				payload := strings.Replace(sseOK, `"prompt_tokens":1,"completion_tokens":1,"total_tokens":2`, tc.usage, 1)
				up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, payload, true })
				h := newTestHandler(t, Config{Pool: testPoolWith(&auth.Auth{UID: "a", AccessToken: "fixture", ExpiresAt: 9999999999}), Upstream: up})
				out := httptest.NewRecorder()
				h.ServeHTTP(out, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"glm-5.2","stream":%v,"messages":[]}`, stream))))
				if out.Code != 200 {
					t.Fatalf("model response=%d", out.Code)
				}
				s, err := h.cfg.Traffic.Snapshot(context.Background(), time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				for _, pair := range [][2]string{{"total", "all"}, {"account", "a"}, {"proxy", "direct"}} {
					r := trafficRow(t, s, pair[0], pair[1])
					if tc.known {
						if r.CacheRate == nil || *r.CacheRate != tc.rate || r.CacheSamples != 1 {
							t.Fatalf("known cache stats: %+v", r)
						}
					} else if r.CacheRate != nil || r.CacheSamples != 0 {
						t.Fatalf("unknown cache stats: %+v", r)
					}
				}
			})
		}
	}
}
