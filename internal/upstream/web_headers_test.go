package upstream

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/device"
)

// webTestClient 是一个只用来构造请求头的客户端（不发网络请求）。
func webTestClient() *Client {
	c := New()
	c.WebBaseCN = "https://www.workbuddy.cn"
	c.BillingBaseGlob = "https://www.workbuddy.ai"
	return c
}

func macAuth(enterprise string) *auth.Auth {
	return &auth.Auth{UID: "u1", EnterpriseID: enterprise, Domain: "www.workbuddy.cn", AccessToken: "at",
		DeviceProfile: device.Profile{Version: 1, Platform: "macos", Arch: "arm64", OSVersion: "24.6.0", CPUCores: 8, MemoryGiB: 16, Timezone: "Asia/Shanghai"}}
}

func TestWebHeadersUseBusinessPageAndDropAPIShape(t *testing.T) {
	c := webTestClient()
	for _, tc := range []struct{ path, wantReferer, wantLanguage string }{
		{"/activity/growth/tasks/t1/claim", "https://www.workbuddy.cn/profile/growth-center", "zh-CN"},
		{"/v2/activity/growth/tasks/accept", "https://www.workbuddy.cn/profile/growth-center", "zh-CN"},
		{"/billing/meter/claim-gift", "https://www.workbuddy.cn/profile/usage", "zh-CN,zh;q=0.9"},
		{"/billing/meter/claim-compensation", "https://www.workbuddy.cn/profile/usage", "zh-CN,zh;q=0.9"},
	} {
		req, _ := http.NewRequest(http.MethodPost, c.webBase()+tc.path, nil)
		c.applyWebHeaders(req, macAuth("ent-1"), tc.path, "")
		if got := req.Header.Get("Referer"); got != tc.wantReferer {
			t.Errorf("%s referer=%q want %q", tc.path, got, tc.wantReferer)
		}
		if got := req.Header.Get("Accept-Language"); got != tc.wantLanguage {
			t.Errorf("%s language=%q want %q", tc.path, got, tc.wantLanguage)
		}
		if req.Header.Get("X-Client-Platform") != "web" {
			t.Errorf("%s platform=%q", tc.path, req.Header.Get("X-Client-Platform"))
		}
		if req.Header.Get("Origin") != "https://www.workbuddy.cn" {
			t.Errorf("%s origin=%q", tc.path, req.Header.Get("Origin"))
		}
		// 网页不会发后台服务请求框架的那几个头。
		for _, name := range []string{"X-User-Id", "X-Tenant-Id", "X-Domain", "X-Product", "X-Request-ID", "X-Requested-With"} {
			if v := req.Header.Get(name); v != "" {
				t.Errorf("%s kept %s=%q", tc.path, name, v)
			}
		}
		if req.Header.Get("X-Enterprise-Id") != "ent-1" {
			t.Errorf("%s enterprise=%q", tc.path, req.Header.Get("X-Enterprise-Id"))
		}
		// 浏览器自动带的元数据头 + Chrome 那组编码。
		if req.Header.Get("Sec-Fetch-Site") != "same-origin" || req.Header.Get("Sec-Fetch-Mode") != "cors" || req.Header.Get("Sec-Fetch-Dest") != "empty" {
			t.Errorf("%s sec-fetch=%v", tc.path, req.Header)
		}
		if req.Header.Get("Sec-CH-UA") != chromeCHUA || req.Header.Get("Sec-CH-UA-Platform") != `"macOS"` {
			t.Errorf("%s client hints=%v", tc.path, req.Header)
		}
		if got := req.Header.Get("Accept-Encoding"); got != acceptEncodingChrome {
			t.Errorf("%s encoding=%q want %q", tc.path, got, acceptEncodingChrome)
		}
		// 网页请求不带 Authorization，靠 Cookie 认人。
		if v := req.Header.Get("Authorization"); v != "" {
			t.Errorf("%s leaked Authorization", tc.path)
		}
	}
	// GET 不带 Origin。
	get, _ := http.NewRequest(http.MethodGet, c.webBase()+"/activity/growth/tasks", nil)
	c.applyWebHeaders(get, macAuth(""), "/activity/growth/tasks", "")
	if get.Header.Get("Origin") != "" {
		t.Errorf("GET origin=%q", get.Header.Get("Origin"))
	}
}

func TestWebHeadersFollowDevicePlatform(t *testing.T) {
	c := webTestClient()
	windows := &auth.Auth{UID: "u2"}
	req, _ := http.NewRequest(http.MethodPost, c.webBase()+"/activity/growth/tasks", nil)
	c.applyWebHeaders(req, windows, "/activity/growth/tasks", "")
	if got := req.Header.Get("Sec-CH-UA-Platform"); got != `"Windows"` {
		t.Fatalf("windows hint=%q", got)
	}
	if got := req.Header.Get("User-Agent"); got != webUA {
		t.Fatalf("windows ua=%q", got)
	}
	mac := macAuth("")
	macReq, _ := http.NewRequest(http.MethodPost, c.webBase()+"/activity/growth/tasks", nil)
	c.applyWebHeaders(macReq, mac, "/activity/growth/tasks", "")
	if got := macReq.Header.Get("User-Agent"); got != macBrowserUA {
		t.Fatalf("mac ua=%q", got)
	}
}

// 网页登录码请求取自 Node 内置 fetch：UA 是 "node"，带 undici 自动补的头。
func TestNodeFetchHeadersForLoginCode(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.test/v2/plugin/device/auth/code", nil)
	applyNodeFetchHeaders(req)
	for name, want := range map[string]string{
		"User-Agent":      "node",
		"Accept":          "*/*",
		"Accept-Language": "*",
		"Sec-Fetch-Mode":  "cors",
		"Accept-Encoding": acceptEncodingNodeFetch,
	} {
		if got := req.Header.Get(name); got != want {
			t.Errorf("%s=%q want %q", name, got, want)
		}
	}
	for _, name := range []string{"Sec-CH-UA", "Origin", "Referer"} {
		if v := req.Header.Get(name); v != "" {
			t.Errorf("login code request kept %s=%q", name, v)
		}
	}
}

// 声明按请求类别走：API/后台服务是 axios 那组，网页是 Chrome 那组。
func TestEncodingPerRequestClass(t *testing.T) {
	c := New()
	c.HeaderProfile = "desktop"
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req, _ := http.NewRequest(http.MethodPost, "https://example.test/v2/chat/completions", nil)
	c.commonHeaders(req, a)
	if got := req.Header.Get("Accept-Encoding"); got != acceptEncodingAxios {
		t.Fatalf("api encoding=%q", got)
	}
	res, _ := http.NewRequest(http.MethodPost, "https://example.test"+resourceSummaryAPI, nil)
	c.billingHeaders(res, a)
	if got := res.Header.Get("Accept-Encoding"); got != acceptEncodingAxios || res.Header.Get("User-Agent") != "axios/1.16.1" {
		t.Fatalf("billing encoding=%q ua=%q", got, res.Header.Get("User-Agent"))
	}
	// 没指定类别的出站请求回落到 axios 那组。
	fallback, _ := http.NewRequest(http.MethodGet, "https://example.test/any", nil)
	if _, err := doUpstream(&http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Accept-Encoding"); got != acceptEncodingAxios {
			t.Errorf("fallback encoding=%q", got)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
	})}, fallback); err != nil {
		t.Fatal(err)
	}
}

// 小程序 web-view 是 iOS WebKit 运行时：编码组、平台标记、UA 都要对得上，
// 且没有桌面 Chrome 的 UA-CH / Sec-Fetch-*。
func TestMPHeadersUseWebKitClass(t *testing.T) {
	c := webTestClient()
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/report", nil)
	c.applyMPHeaders(req, macAuth("ent-mp"), "/v2/report")
	if got := req.Header.Get("Accept-Encoding"); got != acceptEncodingWebKit {
		t.Fatalf("mp encoding=%q want %q", got, acceptEncodingWebKit)
	}
	if strings.Contains(req.Header.Get("Accept-Encoding"), "zstd") {
		t.Error("iOS WebKit must not advertise zstd")
	}
	if req.Header.Get("X-Client-Platform") != "miniprogram" {
		t.Errorf("platform=%q", req.Header.Get("X-Client-Platform"))
	}
	if req.Header.Get("Authorization") != "Bearer at" {
		t.Errorf("authorization=%q", req.Header.Get("Authorization"))
	}
	if ua := req.Header.Get("User-Agent"); !strings.Contains(ua, "iPhone") || !strings.Contains(ua, "MicroMessenger") {
		t.Errorf("ua=%q", ua)
	}
	if req.Header.Get("Referer") != "https://www.workbuddy.cn/profile/growth-center" {
		t.Errorf("referer=%q", req.Header.Get("Referer"))
	}
	if req.Header.Get("X-Enterprise-Id") != "ent-mp" {
		t.Errorf("enterprise=%q", req.Header.Get("X-Enterprise-Id"))
	}
	for _, name := range []string{"Sec-CH-UA", "Sec-CH-UA-Platform", "Sec-Fetch-Site", "X-User-Id", "X-Tenant-Id", "X-Domain", "X-Product", "X-Request-ID", "X-Requested-With"} {
		if v := req.Header.Get(name); v != "" {
			t.Errorf("mp request kept %s=%q", name, v)
		}
	}
	get, _ := http.NewRequest(http.MethodGet, "https://www.workbuddy.cn/v2/report", nil)
	c.applyMPHeaders(get, macAuth(""), "/v2/report")
	if get.Header.Get("Origin") != "" {
		t.Errorf("GET origin=%q", get.Header.Get("Origin"))
	}
}

// 端到端：小程序上报实际发出的就是小程序那组，不再是请求框架（axios）那组。
func TestMPReportUsesMiniProgramClass(t *testing.T) {
	var got *http.Request
	var body []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		got = r
		body, _ = io.ReadAll(r.Body)
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	a := &auth.Auth{UID: "u", AccessToken: "at"}
	if err := c.ReportMPEvent(a, map[string]any{"eventCode": "chat_request_send"}); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("no request issued")
	}
	if enc := got.Header.Get("Accept-Encoding"); enc != acceptEncodingWebKit {
		t.Fatalf("mp report encoding=%q want %q", enc, acceptEncodingWebKit)
	}
	if got.Header.Get("X-Client-Platform") != "miniprogram" || got.Header.Get("Authorization") != "Bearer at" {
		t.Fatalf("mp report headers=%v", got.Header)
	}
	if got.URL.Path != reportPath {
		t.Fatalf("path=%s", got.URL.Path)
	}
	var events []map[string]any
	if err := json.Unmarshal(body, &events); err != nil || len(events) != 1 {
		t.Fatalf("body=%s err=%v", body, err)
	}
}

// 开学季接口只在小程序里跑，同样走小程序那一组。
func TestSchoolAPIUsesMiniProgramClass(t *testing.T) {
	var got *http.Request
	c := testClient(func(r *http.Request) (*http.Response, error) {
		got = r
		return jsonResp(200, `{"code":0,"data":{"tasks":[]}}`), nil
	})
	if _, _, err := c.SchoolTasks(&auth.Auth{UID: "u", AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Header.Get("Accept-Encoding") != acceptEncodingWebKit || got.Header.Get("X-Client-Platform") != "miniprogram" {
		t.Fatalf("school headers=%v", got.Header)
	}
	if !strings.HasPrefix(got.URL.Path, schoolBase) {
		t.Fatalf("path=%s", got.URL.Path)
	}
}

// 事件里的平台字段必须和请求头的运行时一致：不能一边发 iPhone 微信 UA，
// 一边在正文里写 windows/x64。
func TestMPFingerprintMatchesRuntime(t *testing.T) {
	base := mpEventBase(&auth.Auth{UID: "u"})
	if base["platform"] != "mini_program" {
		t.Fatalf("platform=%v", base["platform"])
	}
	if base["os"] != "ios" || base["osVersion"] != mpIOSVersion || base["arch"] != "arm64" {
		t.Fatalf("mp fingerprint=%v", base)
	}
	if parts := strings.Split(mpIOSVersion, "."); !strings.Contains(mpWebViewUA, strings.ReplaceAll(strings.Join(parts[:2], "."), ".", "_")) {
		t.Fatalf("ua=%q does not carry iOS %s", mpWebViewUA, mpIOSVersion)
	}
}

// Cookie 路径也要带上真实来源页：资料库事件的 pageURL 不是成长中心。
func TestWebReportKeepsRealPageOnCookiePath(t *testing.T) {
	var mu sync.Mutex
	var reports []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/device/auth/code":
			io.WriteString(w, `{"code":0,"data":{"deviceCode":"dc"}}`)
		case "/console/client-login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: r.URL.Query().Get("code"), Path: "/"})
			w.Header().Set("Location", "/profile/growth-center")
			w.WriteHeader(http.StatusFound)
		case "/profile/growth-center":
			io.WriteString(w, "page")
		case reportPath:
			mu.Lock()
			reports = append(reports, r.Header.Clone())
			mu.Unlock()
			io.WriteString(w, `{"code":0,"data":{}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := New()
	c.ClientSettings = DefaultClientSettings()
	c.WebAuthMode = "cookie"
	c.ChatBaseCN, c.BillingBaseCN, c.WebBaseCN = srv.URL, srv.URL, srv.URL
	c.ChatBaseGlobal, c.BillingBaseGlob = srv.URL, srv.URL
	a := &auth.Auth{UID: "u", AccessToken: "at", RefreshToken: "rt", Domain: "www.codebuddy.cn"}
	page := "https://www.workbuddy.cn/library/doc/abc"
	if err := c.ReportWebEvent(a, "library_doc_intro_click", page, "el-1", "按钮"); err != nil {
		t.Fatalf("report: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 {
		t.Fatalf("reports=%d", len(reports))
	}
	h := reports[0]
	if got := h.Get("Referer"); got != page {
		t.Fatalf("cookie-path referer=%q want %q", got, page)
	}
	if h.Get("X-Client-Platform") != "web" {
		t.Fatalf("platform=%q", h.Get("X-Client-Platform"))
	}
	if h.Get("Cookie") == "" {
		t.Fatal("cookie session was not used")
	}
}

// compress 是 axios 用 zlib.createUnzip 解的那种编码：gzip 与 zlib 都可能。
func TestCompressContentEncodingDecodesBothWrappers(t *testing.T) {
	payload := []byte(`{"code":0,"data":{"ok":true}}`)
	var zlibBody bytes.Buffer
	zw := zlib.NewWriter(&zlibBody)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var gzipBody bytes.Buffer
	gw := gzip.NewWriter(&gzipBody)
	if _, err := gw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"zlib": zlibBody.Bytes(), "gzip": gzipBody.Bytes()} {
		t.Run(name, func(t *testing.T) {
			c := testClient(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {"compress"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})
			req, _ := http.NewRequest("GET", "https://local.example", nil)
			data, err := c.doJSON(req, &auth.Auth{})
			if err != nil || string(data) != `{"ok":true}` {
				t.Fatalf("data=%s err=%v", data, err)
			}
		})
	}
}
