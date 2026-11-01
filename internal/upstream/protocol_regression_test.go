package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestWebsiteLoginTargetMatchesBusinessReferer(t *testing.T) {
	for _, tc := range []struct{ path, page string }{
		{"/billing/meter/claim-gift", webPageUsage},
		{"/billing/meter/claim-compensation", webPageUsage},
		{"/activity/growth/tasks/accept", webPageGrowthCenter},
	} {
		for _, domain := range []string{"www.workbuddy.cn", "www.workbuddy.ai"} {
			t.Run(domain+tc.path, func(t *testing.T) {
				target, referer := "", ""
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v2/plugin/device/auth/code":
						_, _ = w.Write([]byte(`{"code":0,"data":{"deviceCode":"fixture-code"}}`))
					case "/console/client-login":
						target = r.URL.Query().Get("target")
						http.SetCookie(w, &http.Cookie{Name: "session", Value: "fixture-cookie", Path: "/"})
						_, _ = w.Write([]byte("page"))
					case tc.path:
						referer = r.Header.Get("Referer")
						_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
					default:
						t.Errorf("unexpected path %s", r.URL.Path)
					}
				}))
				defer srv.Close()
				c := New()
				c.HTTP = srv.Client()
				c.ChatBaseCN, c.ChatBaseGlobal = srv.URL, srv.URL
				c.BillingBaseCN, c.BillingBaseGlob, c.WebBaseCN = srv.URL, srv.URL, srv.URL
				c.WebAuthMode = "cookie"
				req, _ := http.NewRequest("POST", srv.URL+tc.path, nil)
				if _, err := c.doJSON(req, &auth.Auth{UID: "fixture-uid", AccessToken: "fixture-token", RefreshToken: "fixture-refresh", Domain: domain}); err != nil {
					t.Fatal(err)
				}
				if target != tc.page || referer != srv.URL+tc.page {
					t.Fatalf("target=%q referer=%q want page=%s", target, referer, tc.page)
				}
			})
		}
	}
}

func TestCustomClientVersionOmitsUnverifiedBuildMetadata(t *testing.T) {
	for _, domain := range []string{"www.workbuddy.cn", "www.workbuddy.ai"} {
		t.Run(domain, func(t *testing.T) {
			var event map[string]any
			c := testClient(func(r *http.Request) (*http.Response, error) {
				var events []map[string]any
				if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
					t.Fatal(err)
				}
				event = events[0]
				return jsonResp(200, `{"code":0,"data":{}}`), nil
			})
			c.ClientSettings = DefaultClientSettings()
			c.ClientVersionCN, c.ClientVersionGlobal = "6.0.1", "6.0.1"
			if err := c.ReportDesktopEvent(&auth.Auth{UID: "fixture-uid", AccessToken: "fixture-token", Domain: domain}, DesktopEvent{"eventCode": "fixture"}); err != nil {
				t.Fatal(err)
			}
			if event["ideVersion"] != "6.0.1" || event["extVersion"] != "6.0.1" {
				t.Fatalf("versions=%v/%v", event["ideVersion"], event["extVersion"])
			}
			for _, field := range []string{"commit", "releaseDate"} {
				if _, exists := event[field]; exists {
					t.Errorf("custom version contains unverified %s=%v", field, event[field])
				}
			}
		})
	}
}
