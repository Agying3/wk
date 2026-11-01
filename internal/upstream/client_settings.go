package upstream

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"workbuddy2api/internal/auth"
)

// ClientSettings is fixed at startup. The empty header profile preserves the
// historical library client; the server explicitly selects desktop defaults.
type ClientSettings struct {
	HeaderProfile        string `json:"header_profile"`
	ClientVersionCN      string `json:"client_version_cn"`
	ClientVersionGlobal  string `json:"client_version_global"`
	CLIVersion           string `json:"cli_version"`
	ModelOptimization    bool   `json:"model_optimization"`
	RefreshIdentityCheck string `json:"refresh_identity_check"`
	WebAuthMode          string `json:"web_auth_mode"`
	ReasoningSummaryAuto bool   `json:"reasoning_summary_auto"`
}

func DefaultClientSettings() ClientSettings {
	return ClientSettings{HeaderProfile: "desktop", ClientVersionCN: "5.9.3", ClientVersionGlobal: "5.9.4", CLIVersion: "2.165.0", RefreshIdentityCheck: "best_effort", WebAuthMode: "auto", ReasoningSummaryAuto: true}
}

var clientVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$`)

func (s ClientSettings) Validate() error {
	if s.WebAuthMode != "" && s.WebAuthMode != "auto" && s.WebAuthMode != "cookie" && s.WebAuthMode != "bearer" {
		return fmt.Errorf("upstream.web_auth_mode must be auto, cookie or bearer")
	}
	if s.HeaderProfile != "" && s.HeaderProfile != "desktop" && s.HeaderProfile != "legacy" {
		return fmt.Errorf("upstream.header_profile must be desktop or legacy")
	}
	for key, v := range map[string]string{"client_version_cn": s.ClientVersionCN, "client_version_global": s.ClientVersionGlobal, "cli_version": s.CLIVersion} {
		if v != "" && (len(v) > 64 || !clientVersionPattern.MatchString(v)) {
			return fmt.Errorf("upstream.%s must be a version number", key)
		}
	}
	switch s.RefreshIdentityCheck {
	case "", "off", "best_effort", "strict":
		return nil
	default:
		return fmt.Errorf("upstream.refresh_identity_check must be off, best_effort or strict")
	}
}

func (c *Client) desktopHeaders() bool { return c.HeaderProfile == "desktop" }

func (c *Client) clientVersion(a *auth.Auth) string {
	v := c.ClientVersionCN
	if a.Region() == auth.RegionGlobal {
		v = c.ClientVersionGlobal
		if v == "" {
			v = DefaultClientSettings().ClientVersionGlobal
		}
	} else if v == "" {
		v = DefaultClientSettings().ClientVersionCN
	}
	return v
}

func (c *Client) desktopUserAgent(a *auth.Auth, chat bool) string {
	if !c.desktopHeaders() {
		return desktopUA
	}
	v, cli := c.clientVersion(a), c.CLIVersion
	if cli == "" {
		cli = DefaultClientSettings().CLIVersion
	}
	first, second := "WorkBuddy", "WorkBuddy"
	if a.Region() == auth.RegionGlobal {
		second = "workbuddy-ai"
		if !chat {
			first = second
		}
	}
	return first + "/" + v + " " + second + "/" + v + " CLI/" + cli
}

func accountHeaders(req *http.Request, a *auth.Auth) {
	s := a.Snapshot()
	if s.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	}
	if s.UID != "" {
		req.Header.Set("X-User-Id", s.UID)
	}
	if s.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", s.EnterpriseID)
		req.Header.Set("X-Tenant-Id", s.EnterpriseID)
	}
	if s.Domain != "" {
		req.Header.Set("X-Domain", s.Domain)
	}
}

func (c *Client) commonHeaders(req *http.Request, a *auth.Auth) {
	if !c.desktopHeaders() {
		CommonHeaders(req, a)
		return
	}
	for _, key := range []string{"Origin", "Referer", "X-CodeBuddy-Request", "X-Machine-ID", "X-Session-ID", "X-Refresh-Token"} {
		req.Header.Del(key)
	}
	if req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", c.desktopUserAgent(a, false))
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Request-ID", NewEventID())
	req.Header.Set("X-Product", "SaaS")
	// 请求框架（axios）的声明；官方没有在哪个请求上改写这个头。
	applyEncoding(req, acceptEncodingAxios)
	accountHeaders(req, a)
	// Department text is optional and must not inject a header delimiter.
	if value := a.Snapshot().DepartmentFullName; value != "" && strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) < 0 {
		req.Header.Set("X-Department-Info", value)
	}
}

func (c *Client) chatHeaders(req *http.Request, a *auth.Auth) {
	if !c.desktopHeaders() {
		ChatHeaders(req, a)
		c.privacyHeaders(req, a)
		return
	}
	c.commonHeaders(req, a)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.desktopUserAgent(a, true))
	if a.Region() == auth.RegionGlobal {
		req.Header.Set("X-Domain", "www.workbuddy.ai")
	}
	for key, value := range map[string]string{"X-Agent-Intent": "craft", "X-Agent-Type": "main", "X-Agent-Purpose": "conversation", "X-Agent-Runtime": "local-agent", "X-IDE-Name": "WorkBuddy", "X-IDE-Type": "WorkBuddy", "X-IDE-Version": c.clientVersion(a)} {
		req.Header.Set(key, value)
	}
	c.privacyHeaders(req, a)
	if a.Snapshot().DeviceProfile.IsMac() {
		// Values verified for the referenced macOS desktop/embedded CLI build.
		// This describes that SDK profile; it does not change gateway deadlines.
		for key, value := range map[string]string{"X-Stainless-Retry-Count": "0", "X-Stainless-Timeout": "1200", "X-Stainless-Lang": "js", "X-Stainless-Package-Version": "6.25.0", "X-Stainless-OS": "MacOS", "X-Stainless-Arch": "arm64", "X-Stainless-Runtime": "node", "X-Stainless-Runtime-Version": "v22.21.1"} {
			req.Header.Set(key, value)
		}
	}
}

const macBrowserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

func browserUserAgent(a *auth.Auth) string {
	if a.Snapshot().DeviceProfile.IsMac() {
		return macBrowserUA
	}
	return webUA
}

func (c *Client) privacyHeaders(req *http.Request, a *auth.Auth) {
	s := a.Snapshot()
	private := !c.ModelOptimization || s.EnterpriseID != "" || s.AccountType == "ultimate" || s.AccountType == "exclusive"
	if private {
		req.Header.Set("X-Private-Data", "true")
	} else {
		req.Header.Set("X-Private-Data", "false")
	}
}

// Only the documented AuthService credit/checkin endpoints use axios headers.
// Other billing/growth paths keep their historical header contract.
func (c *Client) billingHeaders(req *http.Request, a *auth.Auth) {
	BillingHeaders(req, a)
	if !c.desktopHeaders() {
		return
	}
	switch req.URL.Path {
	case "/v2/billing/meter/get-user-resource", "/v2/billing/meter/daily-checkin":
		req.Header.Set("User-Agent", "axios/1.16.1")
		req.Header.Set("X-Request-ID", NewEventID())
		req.Header.Set("Accept", "application/json")
		applyEncoding(req, acceptEncodingAxios)
		if req.URL.Path == "/v2/billing/meter/get-user-resource" {
			language := "zh-CN,zh;q=0.9"
			if a.Region() == auth.RegionGlobal {
				language = "en-US,en;q=0.9"
			}
			req.Header.Set("Accept-Language", language)
		}
	case resourceSummaryAPI, resourceFreeAPI, resourcePaidAPI:
		req.Header.Set("User-Agent", "axios/1.16.1")
		req.Header.Set("Accept", "application/json")
		applyEncoding(req, acceptEncodingAxios)
		language := "zh"
		if a.Region() == auth.RegionGlobal {
			language = "en"
		}
		req.Header.Set("Accept-Language", language)
	default:
		req.Header.Set("User-Agent", c.desktopUserAgent(a, false))
		applyEncoding(req, acceptEncodingAxios)
	}
}

func (c *Client) refreshHeaders(req *http.Request, a *auth.Auth) {
	if !c.desktopHeaders() {
		RefreshHeaders(req, a)
		return
	}
	c.commonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.Snapshot().RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}
