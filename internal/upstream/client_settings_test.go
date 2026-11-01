package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestClientHeaderProfiles(t *testing.T) {
	for _, region := range []string{"cn", "global"} {
		t.Run(region, func(t *testing.T) {
			c := &Client{ClientSettings: DefaultClientSettings()}
			a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", UID: "u", Domain: "copilot.tencent.com"}
			version, chatUA, daemonUA := "5.9.3", "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0", "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0"
			if region == "global" {
				a.Domain = "copilot.workbuddy.ai"
				version, chatUA, daemonUA = "5.9.4", "WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0", "workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0"
			}
			req, _ := http.NewRequest("POST", "https://example.test/v2/chat/completions", strings.NewReader("{}"))
			req.Header.Set("Origin", "https://legacy.example")
			req.Header.Set("X-Refresh-Token", "must-not-leak")
			c.chatHeaders(req, a)
			if req.Header.Get("User-Agent") != chatUA || req.Header.Get("X-IDE-Version") != version || req.Header.Get("Accept") != "application/json" || req.Header.Get("X-Private-Data") != "true" {
				t.Fatalf("incorrect chat profile: %v", req.Header)
			}
			if region == "global" && req.Header.Get("X-Domain") != "www.workbuddy.ai" {
				t.Fatal("global chat domain")
			}
			for _, key := range []string{"Origin", "Referer", "X-CodeBuddy-Request", "X-Machine-ID", "X-Session-ID", "X-Refresh-Token", "X-Stainless-OS"} {
				if req.Header.Get(key) != "" {
					t.Errorf("unexpected %s", key)
				}
			}
			// 请求框架（axios）那组声明，按请求类别给，不再由 doUpstream 统一补。
			if got := req.Header.Get("Accept-Encoding"); got != acceptEncodingAxios {
				t.Errorf("chat encoding=%q want %q", got, acceptEncodingAxios)
			}
			firstID := req.Header.Get("X-Request-ID")
			refresh, _ := http.NewRequest("POST", "https://example.test/v2/plugin/auth/token/refresh", strings.NewReader("{}"))
			c.refreshHeaders(refresh, a)
			if refresh.Header.Get("User-Agent") != daemonUA || refresh.Header.Get("X-Refresh-Token") != "rt" || refresh.Header.Get("X-Auth-Refresh-Source") != "plugin" || refresh.Header.Get("X-IDE-Version") != "" || refresh.Header.Get("X-Request-ID") == firstID {
				t.Fatal("incorrect daemon/refresh profile")
			}
			billing, _ := http.NewRequest("POST", "https://example.test/v2/billing/meter/get-user-resource", strings.NewReader("{}"))
			c.billingHeaders(billing, a)
			if billing.Header.Get("User-Agent") != "axios/1.16.1" || billing.Header.Get("Accept-Language") == "" {
				t.Fatal("incorrect AuthService profile")
			}
			for _, key := range []string{"X-Product", "X-Requested-With", "X-Department-Info", "X-IDE-Version", "X-Refresh-Token"} {
				if billing.Header.Get(key) != "" {
					t.Errorf("billing has %s", key)
				}
			}
		})
	}
}

func TestClientPrivacyAndVersionOverrides(t *testing.T) {
	c := &Client{ClientSettings: DefaultClientSettings()}
	c.ModelOptimization, c.ClientVersionCN, c.CLIVersion = true, "6.0.1", "3.0.0"
	for _, tc := range []struct{ enterprise, accountType, want string }{{"", "", "false"}, {"e", "", "true"}, {"", "ultimate", "true"}, {"", "exclusive", "true"}} {
		a := &auth.Auth{EnterpriseID: tc.enterprise, Authority: auth.Authority{AccountType: tc.accountType}}
		req, _ := http.NewRequest("POST", "https://example.test", nil)
		c.chatHeaders(req, a)
		if req.Header.Get("X-Private-Data") != tc.want || req.Header.Get("X-IDE-Version") != "6.0.1" || !strings.Contains(req.Header.Get("User-Agent"), "CLI/3.0.0") {
			t.Fatal("privacy/version override")
		}
	}
	c.ClientVersionCN = "5.9.3\r\nX-Bad: yes"
	if c.ClientSettings.Validate() == nil {
		t.Fatal("accepted header injection")
	}
}

func TestDesktopReportUsesRegionalVersion(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		var events []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0]["ideVersion"] != "5.9.4" || events[0]["extVersion"] != "5.9.4" {
			t.Fatal("event versions diverged")
		}
		// 国际版号必须带国际版安装包的构建信息，不能沿用 5.5.6 或国内版的值。
		if events[0]["commit"] != desktopCommitGlobal {
			t.Fatalf("commit=%v", events[0]["commit"])
		}
		if events[0]["releaseDate"] != float64(desktopReleaseDateGlobal) {
			t.Fatalf("releaseDate=%v", events[0]["releaseDate"])
		}
		if r.Header.Get("User-Agent") != "workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0" || r.Header.Get("Content-Type") != "application/json;charset=UTF-8" || r.Header.Get("X-Enterprise-Id") != "e" || r.Header.Get("X-Tenant-Id") != "e" {
			t.Fatal("incorrect report headers")
		}
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	c.ClientSettings = DefaultClientSettings()
	a := &auth.Auth{UID: "u", EnterpriseID: "e", AccessToken: "at", Domain: "www.workbuddy.ai"}
	if err := c.ReportDesktopEvent(a, DesktopEvent{"eventCode": "test"}); err != nil {
		t.Fatal(err)
	}
}

// 构建信息按域取对应安装包的值；legacy 模式保持历史形状不变。
func TestDesktopReportBuildInfoFollowsRegion(t *testing.T) {
	for _, tc := range []struct {
		name, domain, commit string
		date                 int64
	}{
		{"cn", "www.workbuddy.cn", desktopCommitCN, desktopReleaseDateCN},
		{"global", "www.workbuddy.ai", desktopCommitGlobal, desktopReleaseDateGlobal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			c := testClient(func(r *http.Request) (*http.Response, error) {
				var events []map[string]any
				if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
					t.Fatal(err)
				}
				got = events[0]
				return jsonResp(200, `{"code":0,"data":{}}`), nil
			})
			c.ClientSettings = DefaultClientSettings()
			if err := c.ReportDesktopEvent(&auth.Auth{UID: "u", AccessToken: "at", Domain: tc.domain}, DesktopEvent{"eventCode": "test"}); err != nil {
				t.Fatal(err)
			}
			if got["commit"] != tc.commit || got["releaseDate"] != float64(tc.date) {
				t.Fatalf("build info=%v/%v", got["commit"], got["releaseDate"])
			}
		})
	}
	var legacy map[string]any
	c := testClient(func(r *http.Request) (*http.Response, error) {
		var events []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
			t.Fatal(err)
		}
		legacy = events[0]
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	c.ClientSettings = ClientSettings{HeaderProfile: "legacy"}
	if err := c.ReportDesktopEvent(&auth.Auth{UID: "u", AccessToken: "at"}, DesktopEvent{"eventCode": "test"}); err != nil {
		t.Fatal(err)
	}
	if legacy["commit"] != "5f9692923c93033111c51ad7b003eb80204a9b75" {
		t.Fatalf("legacy commit=%v", legacy["commit"])
	}
}
