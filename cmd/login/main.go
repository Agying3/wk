// Command login implements the two-step WorkBuddy OAuth device flow used by
// the management console and login.sh.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/endpoints"
	"workbuddy2api/internal/upstream"
)

const (
	upstreamBaseCN      = endpoints.LegacyChatCN
	upstreamBaseGlobal  = endpoints.Global
	originRefererCN     = endpoints.LegacyBillingCN
	originRefererGlobal = endpoints.Global
	legacyClientUA      = "CLI/2.63.2 CodeBuddy/2.63.2"
	desktopFlow         = "desktop"
	legacyStateFile     = "/tmp/wb2api-login-state.json"
	maxResponseBytes    = 1 << 20
)

// 登录请求的形状按官方后台服务发：请求框架（axios）的公共头，UA 是
// <应用名>/<版本> <应用名>/<版本> CLI/<cli>。Accept-Encoding 与 upstream 的
// axios 组一致，所以响应必须自己按 Content-Encoding 解码。
const loginAcceptEncoding = upstream.AcceptEncodingAxios

type loginRegion struct {
	Name          string
	BaseURL       string
	Origin        string
	DefaultDomain string
	StateFile     string
}

func (r loginRegion) endpoint(path string) string {
	return strings.TrimRight(r.BaseURL, "/") + path
}

// resolveLoginRegion maps the user-facing region name to the OAuth host and
// browser origin. Empty means CN for backward compatibility. The management
// server passes WB2A_LOGIN_REGION; the standalone command also accepts an
// optional third argument: login <url|poll> [cn|global].
func resolveLoginRegion(raw string) (loginRegion, error) {
	settings, err := (endpoints.Settings{EndpointProfile: os.Getenv("WB2A_ENDPOINT_PROFILE"), ChatBaseCN: os.Getenv("WB2A_CHAT_BASE_CN"), ChatBaseGlobal: os.Getenv("WB2A_CHAT_BASE_GLOBAL"), WebBaseCN: os.Getenv("WB2A_WEB_BASE_CN")}).Resolve()
	if err != nil {
		return loginRegion{}, err
	}
	cnOrigin := originRefererCN
	if settings.EndpointProfile == "product" || os.Getenv("WB2A_WEB_BASE_CN") != "" {
		cnOrigin = settings.WebBaseCN
	}
	if override := os.Getenv("WB2A_LOGIN_ORIGIN_CN"); override != "" {
		cnOrigin = override
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("WB2A_LOGIN_REGION"))
		// WB2A_REGION may be present beside the server, but an account-pool
		// value of all/mixed cannot select one login endpoint.
		if raw == "" {
			envRegion := strings.TrimSpace(os.Getenv("WB2A_REGION"))
			if envRegion != "" && !strings.EqualFold(envRegion, "all") && !strings.EqualFold(envRegion, "mixed") {
				raw = envRegion
			}
		}
	}
	switch strings.ToLower(raw) {
	case "", "cn", "china":
		return newLoginRegion("cn", settings.ChatBaseCN, cnOrigin, ""), nil
	case "global", "overseas", "international", "intl":
		return newLoginRegion("global", settings.ChatBaseGlobal, originRefererGlobal, "www.workbuddy.ai"), nil
	case "all", "mixed":
		return loginRegion{}, fmt.Errorf("login region must be cn or global, not %q", raw)
	default:
		return loginRegion{}, fmt.Errorf("unknown login region %q (want cn or global)", raw)
	}
}

func newLoginRegion(name, baseURL, origin, defaultDomain string) loginRegion {
	stateFile := strings.TrimSpace(os.Getenv("WB2A_LOGIN_STATE_FILE"))
	if stateFile == "" {
		stateFile = filepath.Join(os.TempDir(), "wb2api-login-state-"+name+".json")
	}
	return loginRegion{
		Name:          name,
		BaseURL:       baseURL,
		Origin:        origin,
		DefaultDomain: defaultDomain,
		StateFile:     stateFile,
	}
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type loginState struct {
	State  string `json:"state"`
	Region string `json:"region,omitempty"`
	Flow   string `json:"flow,omitempty"`
	// 登录观测：会话 id、授权页的 journey owner id、这条 trace 的根 span。
	// 同一轮登录的发起授权/轮询/取账号共用它们，命令行 poll 时从 state 文件沿用。
	LoginSessionID string `json:"login_session_id,omitempty"`
	LoginOwnerID   string `json:"login_owner_id,omitempty"`
	RootSpanID     string `json:"root_span_id,omitempty"`
}

// newSessionID 生成登录观测用的会话 id（UUID v4）。
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newRequestID 对应官方请求框架的 X-Request-ID：randomUUID 去掉连字符。
func newRequestID() string { return strings.ReplaceAll(newSessionID(), "-", "") }

// loginTrace 是一次登录的链路：登录会话 id（trace id 就是它去掉连字符）、授权页的
// journey owner id（login_owner_id），以及这条 trace 的根 span。每次登录一条根 span，
// 每类请求（发起授权、轮询令牌、取账号、取账号列表、打开授权页）各一个子 span。
type loginTrace struct {
	SessionID string
	OwnerID   string
	RootSpan  string
}

func newLoginTrace() loginTrace {
	return loginTrace{SessionID: newSessionID(), OwnerID: newSessionID(), RootSpan: newRequestID()[:16]}
}

// orNew 补齐旧 state 缺的字段：没有会话 id 或根 span 时现生成一条。
func (t loginTrace) orNew() loginTrace {
	if strings.TrimSpace(t.SessionID) == "" || len(t.RootSpan) != 16 {
		return newLoginTrace()
	}
	return t
}

// traceID 是链路 id：登录会话 id 去掉连字符（不是 32 位十六进制时取它的哈希）。
func (t loginTrace) traceID() string {
	id := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(t.SessionID), "-", ""))
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		sum := sha256.Sum256([]byte(t.SessionID))
		id = hex.EncodeToString(sum[:16])
	}
	return id
}

// childSpan 由会话 id 与请求类别派生固定的子 span id，同一类请求的多次轮询共用。
func (t loginTrace) childSpan(kind string) string {
	sum := sha256.Sum256([]byte(t.SessionID + "\x00" + kind))
	return hex.EncodeToString(sum[:8])
}

// setRoot 设根 span 的链路头。官方登录与刷新都用 traceService 新开的根 span、没有父
// span，所以 b3 以空的父 span 结尾（…-1-），X-B3-ParentSpanId 是空值。
func (t loginTrace) setRoot(req *http.Request) {
	trace := t.traceID()
	req.Header.Set("X-Trace-ID", trace)
	req.Header.Set("b3", trace+"-"+t.RootSpan+"-1-")
	req.Header.Set("X-B3-TraceId", trace)
	req.Header.Set("X-B3-ParentSpanId", "")
	req.Header.Set("X-B3-SpanId", t.RootSpan)
	req.Header.Set("X-B3-Sampled", "1")
}

// apply 给一类请求加链路头：root 为真时带根 span 的头；traceparent 永远是这类请求
// 自己的子 span（与根 span 不同），国际版发起授权按官方只发 traceparent。
func (t loginTrace) apply(req *http.Request, kind string, root bool) {
	if strings.TrimSpace(t.SessionID) == "" {
		return
	}
	if root {
		t.setRoot(req)
	}
	req.Header.Set("traceparent", "00-"+t.traceID()+"-"+t.childSpan(kind)+"-01")
}

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fatal("usage: login <url|poll|save> [cn|global]")
	}
	regionArg := ""
	if len(os.Args) == 3 {
		regionArg = os.Args[2]
	}
	cfg, err := resolveLoginRegion(regionArg)
	if err != nil {
		fatal("%v", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		fatal("create cookie jar: %v", err)
	}
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	switch os.Args[1] {
	case "url":
		loginURL, err := startLogin(client, cfg)
		if err != nil {
			fail(err)
		}
		fmt.Println(loginURL)
	case "poll":
		result, err := pollLogin(client, cfg)
		if err != nil {
			fail(err)
		}
		out, err := json.Marshal(result)
		if err != nil {
			fatal("encode result failed")
		}
		fmt.Println(string(out))
	case "save":
		if err := saveLogin(os.Stdin); err != nil {
			fail(err)
		}
	default:
		fatal("unknown subcommand %q (want url|poll|save)", os.Args[1])
	}
}

func (r loginRegion) platform() string {
	if r.Name == "global" {
		return "workbuddy-ai"
	}
	return "workbuddy"
}

func (r loginRegion) loginDomain() string {
	u, err := url.Parse(r.Origin)
	if err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return r.DefaultDomain
}

// loginClientSettings 取会话客户端的版本号：默认与服务端一致，可用环境变量覆盖，
// 以便服务端配置了非默认版本时这里不漂移。
func loginClientSettings() upstream.ClientSettings {
	s := upstream.DefaultClientSettings()
	if v := strings.TrimSpace(os.Getenv("WB2A_CLIENT_VERSION_CN")); v != "" {
		s.ClientVersionCN = v
	}
	if v := strings.TrimSpace(os.Getenv("WB2A_CLIENT_VERSION_GLOBAL")); v != "" {
		s.ClientVersionGlobal = v
	}
	if v := strings.TrimSpace(os.Getenv("WB2A_CLI_VERSION")); v != "" {
		s.CLIVersion = v
	}
	return s
}

func (r loginRegion) clientVersion() string {
	s := loginClientSettings()
	if r.Name == "global" {
		return s.ClientVersionGlobal
	}
	return s.ClientVersionCN
}

// userAgent 是官方后台服务的 UA 形状：两段应用名都取产品配置的 applicationName，
// 国内版是 WorkBuddy，国际版是 workbuddy-ai（后台服务没有 CLIENT_INFO_PLATFORM）。
func (r loginRegion) userAgent() string {
	name := "WorkBuddy"
	if r.Name == "global" {
		name = "workbuddy-ai"
	}
	v, cli := r.clientVersion(), loginClientSettings().CLIVersion
	return name + "/" + v + " " + name + "/" + v + " CLI/" + cli
}

func startLogin(client *http.Client, cfg loginRegion) (string, error) {
	trace := newLoginTrace()
	// 国内版发起授权带根 span 的头；国际版官方把根 span 的头放在请求头之外，实际不发。
	includeRoot := cfg.Name != "global"
	data, status, err := doJSON(client, cfg, http.MethodPost, cfg.endpoint("/v2/plugin/auth/state?platform=")+cfg.platform(), func(req *http.Request) {
		anonymousHeaders(req, cfg)
		req.Header.Set("X-Domain", cfg.loginDomain())
		req.Header.Set("X-Custom-Channel-Id", "unknown")
		trace.apply(req, "authorization", includeRoot)
	}, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", failureAt("authorization", status, err)
	}
	var state struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &state); err != nil || state.State == "" || state.AuthURL == "" {
		return "", &auth.LoginFailure{Stage: "authorization"}
	}
	authorization, err := url.Parse(state.AuthURL)
	if err != nil || authorization.Host == "" || authorization.User != nil || (authorization.Scheme != "https" && authorization.Scheme != "http") {
		return "", &auth.LoginFailure{Stage: "authorization"}
	}
	// 授权页地址按官方追加参数：两个域都加客户端版本；国内版另加登录观测的
	// traceparent（打开授权页这一步自己的子 span）、loginSessionId、login_owner_id。
	query := authorization.Query()
	query.Set("version", cfg.clientVersion())
	if cfg.Name != "global" {
		query.Set("traceparent", "00-"+trace.traceID()+"-"+trace.childSpan("auth-page")+"-01")
		query.Set("loginSessionId", trace.SessionID)
		query.Set("login_owner_id", trace.OwnerID)
	}
	authorization.RawQuery = query.Encode()
	raw, err := json.Marshal(loginState{State: state.State, Region: cfg.Name, Flow: desktopFlow,
		LoginSessionID: trace.SessionID, LoginOwnerID: trace.OwnerID, RootSpanID: trace.RootSpan})
	if err != nil {
		return "", fmt.Errorf("encode authorization state failed")
	}
	if dir := filepath.Dir(cfg.StateFile); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create authorization state directory failed")
		}
	}
	if err := os.WriteFile(cfg.StateFile, raw, 0o600); err != nil {
		return "", fmt.Errorf("write authorization state failed")
	}
	_ = os.Chmod(cfg.StateFile, 0o600)
	return authorization.String(), nil
}

func pollLogin(client *http.Client, cfg loginRegion) (map[string]any, error) {
	state, statePath, err := readLoginState(cfg)
	if err != nil {
		return nil, &auth.LoginFailure{Stage: "state"}
	}
	escapedState := url.QueryEscape(state.State)
	trace := loginTrace{SessionID: state.LoginSessionID, OwnerID: state.LoginOwnerID, RootSpan: state.RootSpanID}.orNew()
	requestHeaders := func(req *http.Request) {
		if state.Flow == desktopFlow {
			anonymousHeaders(req, cfg)
			trace.apply(req, "token", true)
		} else {
			legacyHeaders(req, cfg)
		}
	}
	tokenRaw, status, err := doJSON(client, cfg, http.MethodGet, cfg.endpoint("/v2/plugin/auth/token?state=")+escapedState, requestHeaders, nil)
	if err != nil {
		return nil, failureAt("token", status, err)
	}
	var token struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
		TokenType    string `json:"tokenType"`
	}
	if err := json.Unmarshal(tokenRaw, &token); err != nil || token.AccessToken == "" {
		return nil, &auth.LoginFailure{Stage: "token"}
	}

	// 取账号与取账号列表是两类请求，各有自己的子 span。
	headersFor := func(kind string) func(*http.Request) {
		return func(req *http.Request) {
			if state.Flow == desktopFlow {
				commonHeaders(req, cfg)
				req.Header.Set("X-Domain", firstNonEmpty(token.Domain, cfg.loginDomain()))
				if cfg.Name == "global" {
					for _, key := range []string{"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
						req.Header.Set(key, "true")
					}
				}
				trace.apply(req, kind, true)
			} else {
				legacyHeaders(req, cfg)
			}
			req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		}
	}
	headers := headersFor("account")
	accountPath := "/v2/plugin/login/account?state=" + escapedState
	if state.Flow == desktopFlow && cfg.Name == "cn" {
		accountPath = "/v2/plugin/account"
	}
	accountRaw, accountStatus, accountErr := fetchLoginAccount(client, cfg, cfg.endpoint(accountPath), headers, time.Second)
	if accountErr != nil {
		return nil, failureAt("account", accountStatus, accountErr)
	}
	account, err := auth.ParseLoginAccount(accountRaw)
	if err != nil {
		return nil, &auth.LoginFailure{Stage: "account"}
	}
	uid, enterpriseID, nickname := account.UID, account.EnterpriseID, account.Nickname
	if uid == "" {
		return nil, &auth.LoginFailure{Stage: "account"}
	}
	if state.Flow == desktopFlow && cfg.Name == "global" {
		// This snapshot is optional and must not replace the selected account.
		snapshotClient := *client
		if snapshotClient.Timeout == 0 || snapshotClient.Timeout > 5*time.Second {
			snapshotClient.Timeout = 5 * time.Second
		}
		_, _, _ = doJSON(&snapshotClient, cfg, http.MethodGet, cfg.endpoint("/v2/plugin/accounts"), headersFor("accounts"), nil)
	}

	domain := strings.TrimSpace(token.Domain)
	if cfg.Name == "global" && (&auth.Auth{Domain: domain}).Region() != auth.RegionGlobal {
		// Keep the saved credential tied to the selected international host even
		// when the upstream omits domain or returns a legacy alias.
		domain = cfg.DefaultDomain
	} else if domain == "" {
		domain = cfg.DefaultDomain
	}

	result := map[string]any{
		"access_token": token.AccessToken, "refresh_token": token.RefreshToken,
		"expires_in": token.ExpiresIn, "domain": domain, "region": cfg.Name,
		"uid": uid, "enterprise_id": enterpriseID, "nickname": nickname,
		"account_type": account.AccountType, "oneid_account_id": account.OneIDAccountID,
		"idp": account.IDP, "department_full_name": account.DepartmentFullName, "token_type": token.TokenType,
	}
	_ = os.Remove(statePath)
	return result, nil
}

func readLoginState(cfg loginRegion) (loginState, string, error) {
	paths := []string{cfg.StateFile}
	// Migrate the old single CN state file so an authorization started before
	// an upgrade can still be completed.
	if cfg.Name == "cn" && os.Getenv("WB2A_LOGIN_STATE_FILE") == "" {
		paths = append(paths, legacyStateFile)
	}
	var lastErr error
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		var state loginState
		if err := json.Unmarshal(raw, &state); err != nil || strings.TrimSpace(state.State) == "" {
			lastErr = fmt.Errorf("parse state: invalid state file")
			continue
		}
		if state.Region != "" && !strings.EqualFold(state.Region, cfg.Name) {
			return loginState{}, "", fmt.Errorf("state belongs to %s login, current request is %s", state.Region, cfg.Name)
		}
		if state.Flow != "" && state.Flow != desktopFlow {
			return loginState{}, "", fmt.Errorf("unsupported authorization flow; run login url again")
		}
		return state, path, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("state file not found")
	}
	return loginState{}, "", lastErr
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// 令牌就绪后，账号资料可能短暂返回 12151。只重试这个业务状态，
// 最多额外请求五次，整个账号阶段不超过 30 秒；不重新领取令牌。
func fetchLoginAccount(client *http.Client, cfg loginRegion, fullURL string, headers func(*http.Request), retryDelay time.Duration) (json.RawMessage, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for attempt := 0; ; attempt++ {
		raw, status, err := doJSONContext(ctx, client, cfg, http.MethodGet, fullURL, headers, nil)
		var failure *auth.LoginFailure
		if attempt >= 5 || status != http.StatusOK || !errors.As(err, &failure) || failure.Code != 12151 {
			return raw, status, err
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, status, err
		case <-timer.C:
		}
	}
}

func doJSON(client *http.Client, cfg loginRegion, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	return doJSONContext(context.Background(), client, cfg, method, fullURL, headers, body)
}

func doJSONContext(ctx context.Context, client *http.Client, cfg loginRegion, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, 0, fmt.Errorf("create upstream request failed")
	}
	if headers == nil {
		commonHeaders(req, cfg)
	} else {
		headers(req)
	}
	apiClient := *client
	apiClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("upstream connection failed")
	}
	// 我们显式声明了 Accept-Encoding，Go 不再自动解压，这里按 Content-Encoding 解。
	responseBody := io.ReadCloser(resp.Body)
	if decoded, ok := upstream.DecodeResponse(resp.Header.Get("Content-Encoding"), resp.Body); ok {
		responseBody = decoded
	}
	defer responseBody.Close()
	raw, readErr := io.ReadAll(io.LimitReader(responseBody, maxResponseBytes+1))
	if readErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("read upstream response failed")
	}
	if len(raw) > maxResponseBytes {
		return nil, resp.StatusCode, fmt.Errorf("upstream response too large")
	}
	var envelope apiEnvelope
	parseErr := json.Unmarshal(raw, &envelope)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || (parseErr == nil && envelope.Code != 0) {
		return nil, resp.StatusCode, &auth.LoginFailure{Status: resp.StatusCode, Code: envelope.Code}
	}
	if parseErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("invalid upstream response")
	}
	return envelope.Data, resp.StatusCode, nil
}

// commonHeaders 是官方请求框架的公共头（axios）：Accept、X-Requested-With、
// X-Request-ID、X-Product，UA 是后台服务的。官方这两类登录请求不带 Origin/Referer。
func commonHeaders(req *http.Request, cfg loginRegion) {
	if req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Request-ID", newRequestID())
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", cfg.userAgent())
	req.Header.Set("Accept-Encoding", loginAcceptEncoding)
}

func anonymousHeaders(req *http.Request, cfg loginRegion) {
	commonHeaders(req, cfg)
	for _, key := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		req.Header.Set(key, "true")
	}
}

// legacyHeaders 保留升级前 CLI 形状（旧 state 继续用它完成登录），不随本次对齐改动。
func legacyHeaders(req *http.Request, cfg loginRegion) {
	if req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", cfg.Origin)
	req.Header.Set("Referer", strings.TrimRight(cfg.Origin, "/")+"/")
	req.Header.Set("User-Agent", legacyClientUA)
}

func failureAt(stage string, status int, err error) error {
	failure := &auth.LoginFailure{Stage: stage, Status: status}
	var upstreamFailure *auth.LoginFailure
	if errors.As(err, &upstreamFailure) {
		failure.Code = upstreamFailure.Code
	}
	return failure
}

func fail(err error) {
	var failure *auth.LoginFailure
	if errors.As(err, &failure) {
		_ = json.NewEncoder(os.Stderr).Encode(failure)
		if failure.Pending() {
			os.Exit(2)
		}
		os.Exit(1)
	}
	fatal("%v", err)
}

func fatal(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}
