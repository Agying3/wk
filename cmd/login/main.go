// Command login implements the two-step WorkBuddy OAuth device flow used by
// the management console and login.sh.
package main

import (
	"bytes"
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
)

const (
	upstreamBaseCN      = endpoints.LegacyChatCN
	upstreamBaseGlobal  = endpoints.Global
	originRefererCN     = endpoints.LegacyBillingCN
	originRefererGlobal = endpoints.Global
	legacyClientUA      = "CLI/2.63.2 CodeBuddy/2.63.2"
	clientUA            = "workbuddy2api-login"
	desktopFlow         = "desktop"
	legacyStateFile     = "/tmp/wb2api-login-state.json"
	maxResponseBytes    = 1 << 20
)

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
}

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fatal("usage: login <url|poll> [cn|global]")
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
	default:
		fatal("unknown subcommand %q (want url|poll)", os.Args[1])
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

func startLogin(client *http.Client, cfg loginRegion) (string, error) {
	data, status, err := doJSON(client, cfg, http.MethodPost, cfg.endpoint("/v2/plugin/auth/state?platform=")+cfg.platform(), func(req *http.Request) {
		anonymousHeaders(req, cfg)
		req.Header.Set("X-Domain", cfg.loginDomain())
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
	raw, err := json.Marshal(loginState{State: state.State, Region: cfg.Name, Flow: desktopFlow})
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
	requestHeaders := func(req *http.Request) {
		if state.Flow == desktopFlow {
			anonymousHeaders(req, cfg)
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

	headers := func(req *http.Request) {
		if state.Flow == desktopFlow {
			commonHeaders(req, cfg)
			req.Header.Set("X-Domain", firstNonEmpty(token.Domain, cfg.loginDomain()))
			if cfg.Name == "global" {
				for _, key := range []string{"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
					req.Header.Set(key, "true")
				}
			}
		} else {
			legacyHeaders(req, cfg)
		}
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}
	accountPath := "/v2/plugin/login/account?state=" + escapedState
	if state.Flow == desktopFlow && cfg.Name == "cn" {
		accountPath = "/v2/plugin/account"
	}
	accountRaw, accountStatus, accountErr := doJSON(client, cfg, http.MethodGet, cfg.endpoint(accountPath), headers, nil)
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
		_, _, _ = doJSON(&snapshotClient, cfg, http.MethodGet, cfg.endpoint("/v2/plugin/accounts"), headers, nil)
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

func doJSON(client *http.Client, cfg loginRegion, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
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
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
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

func commonHeaders(req *http.Request, cfg loginRegion) {
	if req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", clientUA)
}

func anonymousHeaders(req *http.Request, cfg loginRegion) {
	commonHeaders(req, cfg)
	for _, key := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		req.Header.Set(key, "true")
	}
}

func legacyHeaders(req *http.Request, cfg loginRegion) {
	commonHeaders(req, cfg)
	req.Header.Set("Content-Type", "application/json")
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
