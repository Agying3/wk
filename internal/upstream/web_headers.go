// web_headers.go 官网网页（浏览器）与网页登录码请求的请求头形状。
//
// 依据：官网网页在用户的桌面 Chrome 里跑，网页登录码由后台服务主进程用 Node 内置
// fetch 发。两者与请求框架（axios）的形状不同，所以单独收在这里，避免和 API 请求
// 的头混用。
//
// 未抓包确认的部分：Sec-CH-UA 的值按 Chromium 的 GREASE 规则由主版本 152 推算；
// Sec-Fetch-* 按同源 XHR / 顶层导航的语义给。业务字段与页面归属来自官网前端代码。
package upstream

import (
	"net/http"
	"net/url"
	"strings"

	"workbuddy2api/internal/auth"
)

// chromeCHUA 与 macBrowserUA 的 Chrome 15x 主版本对应；未抓包确认。
const chromeCHUA = `"Chromium";v="152", "Not;A=Brand";v="24", "Google Chrome";v="152"`

// mpIOSVersion 小程序 web-view 报的 iOS 版本。UA 里的 17_5 与事件正文的 osVersion
// 都取它，避免两处独立漂移（本包测试会校验 UA 与该常量一致）。
const mpIOSVersion = "17.5.0"

// mpWebViewUA 是微信 iOS web-view（小程序内嵌页）的 UA。iOS 与微信的版本号是挑选的
// 近期值，未抓包；小程序 appid 未核实，所以不带 `miniProgram/<appid>` 段。
const mpWebViewUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 MicroMessenger/8.0.49(0x18003132) NetType/WIFI Language/zh_CN"

// 官网前端各功能所在的页面路径（Referer 用）。
const (
	webPageGrowthCenter = "/profile/growth-center"
	webPageUsage        = "/profile/usage"
	webPageProfile      = "/profile"
	webPageAccount      = "/profile/account-settings"
)

// webPageFor 返回该业务接口在官网前端的来源页面。官网网页的 axios 实例按功能所在
// 页面设 Referer：成长中心是 /profile/growth-center，积分补偿和新手礼包是 /profile/usage。
func webPageFor(path string) string {
	switch {
	case strings.HasSuffix(path, "/claim-gift"), strings.HasSuffix(path, "/claim-compensation"):
		return webPageUsage
	case strings.Contains(path, "/activity/growth"):
		return webPageGrowthCenter
	case strings.HasPrefix(path, "/v2/trial"), strings.HasPrefix(path, "/trial"), path == "/trial":
		return webPageProfile
	case strings.Contains(path, "/account"):
		return webPageAccount
	default:
		return webPageGrowthCenter
	}
}

// originOf 取站点根（scheme://host），用于 Origin 头。
func originOf(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.TrimRight(base, "/")
	}
	return u.Scheme + "://" + u.Host
}

// requestOrigin 取请求自身的目标站点根；解析不出时回落到客户端配置的网页域。
// 网页与小程序两类请求共用它，避免 Referer/Origin 和实际目标域不一致。
func (c *Client) requestOrigin(req *http.Request, a *auth.Auth) string {
	if req.URL != nil && req.URL.Scheme != "" && req.URL.Host != "" {
		return req.URL.Scheme + "://" + req.URL.Host
	}
	return originOf(c.websiteBase(a))
}

// webAcceptLanguage 按页面语言给 Accept-Language：成长中心接口由官网拦截器显式设成
// zh-CN 或 en-US，其余请求用 Chrome 自动带的值。
func webAcceptLanguage(a *auth.Auth, path string) string {
	global := a != nil && a.Region() == auth.RegionGlobal
	if strings.Contains(path, "/activity/growth") {
		if global {
			return "en-US"
		}
		return "zh-CN"
	}
	if global {
		return "en-US,en;q=0.9"
	}
	return "zh-CN,zh;q=0.9"
}

// webClientHintPlatform 返回 Chrome 的 UA 平台提示，跟设备档案走而不是网关所在系统，
// 免得出现"Windows 的 UA 配 macOS 的平台提示"。
func webClientHintPlatform(a *auth.Auth) string {
	if a != nil && a.Snapshot().DeviceProfile.IsMac() {
		return `"macOS"`
	}
	return `"Windows"`
}

// applyWebHeaders 设置官网网页业务请求的头：只带官网 axios 实例会发的字段。
// 企业号只有 X-Enterprise-Id；不带 X-User-Id、X-Tenant-Id、X-Domain、X-Product、
// X-Request-ID、X-Requested-With（这些是后台服务请求框架的头，网页不发）。
// page 为真实来源页地址；为空时按业务路径推断（见 webPageFor）。
// Authorization 不在这里设：网页靠 Cookie 认人，只有退回 bearer 时才由调用方补。
func (c *Client) applyWebHeaders(req *http.Request, a *auth.Auth, path, page string) {
	origin := c.requestOrigin(req, a)
	if page == "" {
		page = origin + webPageFor(path)
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", browserUserAgent(a))
	req.Header.Set("X-Client-Platform", "web")
	req.Header.Set("Referer", page)
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		req.Header.Set("Origin", origin)
	}
	req.Header.Set("Accept-Language", webAcceptLanguage(a, path))
	// 浏览器自动带的元数据头：同源 XHR。
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-CH-UA", chromeCHUA)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	req.Header.Set("Sec-CH-UA-Platform", webClientHintPlatform(a))
	if id := a.Snapshot().EnterpriseID; id != "" {
		req.Header.Set("X-Enterprise-Id", id)
	}
	applyEncoding(req, acceptEncodingChrome)
}

// applyMPHeaders 设置小程序 web-view 通道的请求头：同一份官网网页，平台标
// miniprogram、用 Bearer 认人，运行时是 iOS WebKit（不是桌面 Chrome，也没有 UA-CH）。
func (c *Client) applyMPHeaders(req *http.Request, a *auth.Auth, path string) {
	s := a.Snapshot()
	origin := c.requestOrigin(req, a)
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", mpWebViewUA)
	req.Header.Set("X-Client-Platform", "miniprogram")
	req.Header.Set("Referer", origin+webPageFor(path))
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		req.Header.Set("Origin", origin)
	}
	req.Header.Set("Accept-Language", "zh-CN")
	if s.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", s.EnterpriseID)
	}
	// iOS WebKit 不发 zstd，也没有 sec-ch-ua；声明与运行时一致。
	applyEncoding(req, acceptEncodingWebKit)
}

// applyWebNavigationHeaders 设置浏览器顶层导航（打开 client-login 页）的头。
func applyWebNavigationHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("User-Agent", browserUserAgent(a))
	req.Header.Set("Accept-Language", webAcceptLanguage(a, ""))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Sec-CH-UA", chromeCHUA)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	req.Header.Set("Sec-CH-UA-Platform", webClientHintPlatform(a))
	applyEncoding(req, acceptEncodingChrome)
}

// applyNodeFetchHeaders 设置 Node 内置 fetch（undici）自动加的头，用于网页登录码请求。
// 这些值取自 undici 的默认行为：登录码那次请求官方没有设 UA，undici 补 "node"。
func applyNodeFetchHeaders(req *http.Request) {
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "*")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("User-Agent", "node")
	applyEncoding(req, acceptEncodingNodeFetch)
}
