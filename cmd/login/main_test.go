package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestResolveLoginRegion(t *testing.T) {
	t.Setenv("WB2A_LOGIN_REGION", "")
	t.Setenv("WB2A_REGION", "all")
	t.Setenv("WB2A_LOGIN_STATE_FILE", "")

	cn, err := resolveLoginRegion("")
	if err != nil {
		t.Fatalf("resolve cn: %v", err)
	}
	if cn.Name != "cn" || cn.BaseURL != upstreamBaseCN || cn.Origin != originRefererCN {
		t.Fatalf("cn config=%+v", cn)
	}

	global, err := resolveLoginRegion("overseas")
	if err != nil {
		t.Fatalf("resolve global: %v", err)
	}
	if global.Name != "global" || global.BaseURL != upstreamBaseGlobal || global.Origin != originRefererGlobal || global.DefaultDomain != "www.workbuddy.ai" {
		t.Fatalf("global config=%+v", global)
	}
	if !strings.Contains(filepath.Base(global.StateFile), "global") {
		t.Fatalf("global state file=%q", global.StateFile)
	}

	if _, err := resolveLoginRegion("all"); err == nil {
		t.Fatal("all must not select a single OAuth endpoint")
	}
}

func TestCommonHeadersUseRegion(t *testing.T) {
	cfg := loginRegion{Name: "global", Origin: "https://www.workbuddy.ai"}
	req := httptest.NewRequest(http.MethodGet, "https://example.test", nil)
	commonHeaders(req, cfg)
	// 官方后台服务的这两类登录请求不发 Origin/Referer，UA 是后台服务形状。
	if req.Header.Get("Origin") != "" || req.Header.Get("Referer") != "" {
		t.Fatalf("login must not send browser provenance headers: %v", req.Header)
	}
	if got, want := req.Header.Get("User-Agent"), cfg.userAgent(); got != want {
		t.Fatalf("ua=%q want %q", got, want)
	}
	if !strings.HasPrefix(req.Header.Get("User-Agent"), "workbuddy-ai/") || !strings.Contains(req.Header.Get("User-Agent"), " CLI/") {
		t.Fatalf("ua shape=%q", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("X-Requested-With") != "XMLHttpRequest" || req.Header.Get("X-Product") != "SaaS" {
		t.Fatalf("framework headers=%v", req.Header)
	}
	if id := req.Header.Get("X-Request-ID"); len(id) != 32 || strings.Contains(id, "-") {
		t.Fatalf("request id=%q", id)
	}
	if req.Header.Get("Accept-Encoding") != loginAcceptEncoding {
		t.Fatalf("encoding=%q", req.Header.Get("Accept-Encoding"))
	}
}

func TestReadLoginStateChecksRegion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"state":"s1","region":"global"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loginRegion{Name: "global", StateFile: path}
	state, gotPath, err := readLoginState(cfg)
	if err != nil || state.State != "s1" || gotPath != path {
		t.Fatalf("state=%+v path=%q err=%v", state, gotPath, err)
	}
	if _, _, err := readLoginState(loginRegion{Name: "cn", StateFile: path}); err == nil {
		t.Fatal("region mismatch should fail")
	}
}

func TestDoJSONUsesConfiguredEndpointAndHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/state" || r.URL.Query().Get("platform") != "workbuddy-ai" {
			t.Errorf("path=%s query=%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Origin") != "" {
			t.Errorf("origin=%q", r.Header.Get("Origin"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "{}" {
			t.Errorf("body=%q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"state":"s","authUrl":"https://www.workbuddy.ai/login"}}`))
	}))
	defer srv.Close()
	cfg := loginRegion{BaseURL: srv.URL, Origin: "https://www.workbuddy.ai"}
	data, status, err := doJSON(http.DefaultClient, cfg, http.MethodPost, cfg.endpoint("/v2/plugin/auth/state?platform=workbuddy-ai"), nil, strings.NewReader("{}"))
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if !strings.Contains(string(data), `"state":"s"`) {
		t.Fatalf("data=%s", data)
	}
}

func TestDesktopLoginLifecycle(t *testing.T) {
	for _, region := range []string{"cn", "global"} {
		t.Run(region, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			var cfg loginRegion
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.URL.Path)
				if r.Header.Get("User-Agent") != cfg.userAgent() || r.Header.Get("Origin") != "" || r.Header.Get("Referer") != "" || r.Header.Get("X-Refresh-Token") != "" {
					t.Error("unexpected login request headers")
				}
				if r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("X-Product") != "SaaS" || len(r.Header.Get("X-Request-ID")) != 32 {
					t.Errorf("framework headers missing: %v", r.Header)
				}
				if r.Header.Get("traceparent") == "" {
					t.Error("login request lost its trace context")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v2/plugin/auth/state":
					platform := "workbuddy"
					if region == "global" {
						platform = "workbuddy-ai"
					}
					if r.Method != "POST" || r.URL.Query().Get("platform") != platform || r.Header.Get("X-Domain") != "login.example.test" {
						t.Error("incorrect authorization request")
					}
					if r.Header.Get("X-Custom-Channel-Id") != "unknown" {
						t.Error("authorization request lost its custom channel id")
					}
					// 国内版发起授权带根 span 的头；国际版官方不发这几个头。
					if region == "cn" && r.Header.Get("X-Trace-ID") == "" {
						t.Error("CN authorization should carry the root span")
					}
					if region == "global" && r.Header.Get("X-Trace-ID") != "" {
						t.Error("global authorization must not send root span headers")
					}
					for _, key := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
						if r.Header.Get(key) != "true" {
							t.Errorf("missing %s", key)
						}
					}
					body, _ := io.ReadAll(r.Body)
					if string(body) != "{}" {
						t.Error("incorrect authorization body")
					}
					io.WriteString(w, `{"code":0,"data":{"state":"s +&","authUrl":"https://login.example.test/auth?state=s"}}`)
				case "/v2/plugin/auth/token":
					if r.URL.Query().Get("state") != "s +&" || r.Header.Get("Authorization") != "" || r.Header.Get("X-No-Authorization") != "true" {
						t.Error("incorrect token request")
					}
					io.WriteString(w, `{"code":0,"data":{"accessToken":"test-at","refreshToken":"test-rt","expiresIn":3600,"domain":"www.workbuddy.ai","tokenType":"Bearer"}}`)
				case "/v2/plugin/account", "/v2/plugin/login/account":
					want := "/v2/plugin/account"
					if region == "global" {
						want = "/v2/plugin/login/account"
					}
					if r.URL.Path != want || r.Header.Get("Authorization") != "Bearer test-at" || r.Header.Get("X-Domain") != "www.workbuddy.ai" {
						t.Error("incorrect account request")
					}
					if region == "cn" && r.URL.RawQuery != "" {
						t.Error("CN account must not use a state query")
					}
					if region == "global" && r.URL.Query().Get("state") != "s +&" {
						t.Error("global account must use original state")
					}
					io.WriteString(w, `{"code":0,"data":{"uid":"u1","type":"personal","oneidAccountId":"oid","idp":"idp1","departmentFullName":"dept"}}`)
				case "/v2/plugin/accounts":
					// The optional snapshot must not fail an otherwise complete login.
					w.WriteHeader(503)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			cfg = loginRegion{Name: region, BaseURL: srv.URL, Origin: "https://login.example.test", StateFile: path}
			gotURL, err := startLogin(srv.Client(), cfg)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			loginURL, err := url.Parse(gotURL)
			if err != nil || loginURL.Host != "login.example.test" || loginURL.Path != "/auth" || loginURL.Query().Get("state") != "s" {
				t.Fatalf("start url=%q err=%v", gotURL, err)
			}
			if loginURL.Query().Get("version") != cfg.clientVersion() {
				t.Fatalf("authorization url lost client version: %q", gotURL)
			}
			if region == "cn" {
				if loginURL.Query().Get("loginSessionId") == "" || loginURL.Query().Get("traceparent") == "" || loginURL.Query().Get("login_owner_id") == "" {
					t.Fatalf("CN authorization url lost login observation: %q", gotURL)
				}
			} else if loginURL.Query().Get("loginSessionId") != "" || loginURL.Query().Get("traceparent") != "" || loginURL.Query().Get("login_owner_id") != "" {
				t.Fatalf("global authorization url must omit login observation: %q", gotURL)
			}
			state, _, err := readLoginState(cfg)
			if err != nil || state.Flow != desktopFlow || state.Region != region {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			if state.LoginSessionID == "" || state.LoginOwnerID == "" || len(state.RootSpanID) != 16 {
				t.Fatalf("state lost login trace fields: %+v", state)
			}
			result, err := pollLogin(srv.Client(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if result["uid"] != "u1" || result["account_type"] != "personal" || result["oneid_account_id"] != "oid" || result["token_type"] != "Bearer" || result["department_full_name"] != "dept" {
				t.Fatal("lost login metadata")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("completed login state was not removed")
			}
			wantRequests := 3
			if region == "global" {
				wantRequests = 4
			}
			if len(requests) != wantRequests {
				t.Fatalf("requests=%v", requests)
			}
		})
	}
}

func TestLegacyAuthorizationStillCompletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"state":"old","region":"cn"}`), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != legacyClientUA || r.Header.Get("Origin") != "https://legacy.example.test" {
			t.Error("legacy request contract changed")
		}
		if r.URL.Path == "/v2/plugin/auth/token" {
			io.WriteString(w, `{"code":0,"data":{"accessToken":"test-at"}}`)
		} else if r.URL.Path == "/v2/plugin/login/account" && r.URL.Query().Get("state") == "old" {
			io.WriteString(w, `{"code":0,"data":{"uid":"u-old"}}`)
		} else {
			t.Error("unexpected legacy request")
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	result, err := pollLogin(srv.Client(), loginRegion{Name: "cn", BaseURL: srv.URL, Origin: "https://legacy.example.test", StateFile: path})
	if err != nil || result["uid"] != "u-old" {
		t.Fatalf("legacy completion err=%v", err)
	}
}

func TestLoginFailuresAreSafeAndOnly11217Waits(t *testing.T) {
	for _, tc := range []struct {
		name, stage, body string
		status, code      int
		pending           bool
	}{
		{"waiting", "token", `{"code":11217,"msg":"secret-test-value"}`, 200, 11217, true},
		{"rejected", "authorization", `{"code":12005,"msg":"secret-test-value"}`, 403, 12005, false},
		{"expired", "token", `{"code":11212,"msg":"secret-test-value"}`, 200, 11212, false},
		{"bad_status", "token", `{"code":11217}`, 503, 11217, false},
		{"invalid_body", "token", `secret-test-value`, 200, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
			defer srv.Close()
			_, status, err := doJSON(srv.Client(), loginRegion{}, "GET", srv.URL, nil, nil)
			if err == nil {
				t.Fatal("expected error")
			}
			failure := failureAt(tc.stage, status, err)
			var got *auth.LoginFailure
			if !errors.As(failure, &got) || got.Code != tc.code || got.Pending() != tc.pending {
				t.Fatalf("failure=%v", failure)
			}
			raw, _ := json.Marshal(got)
			if strings.Contains(failure.Error()+string(raw), "secret-test-value") {
				t.Fatal("provider error body leaked")
			}
		})
	}
}

func TestPendingLoginKeepsStateAndCanCompleteLater(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"state":"pending","region":"cn","flow":"desktop"}`), 0600); err != nil {
		t.Fatal(err)
	}
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/token" {
			polls++
			if polls == 1 {
				io.WriteString(w, `{"code":11217}`)
				return
			}
			io.WriteString(w, `{"code":0,"data":{"accessToken":"test-at"}}`)
			return
		}
		io.WriteString(w, `{"code":0,"data":{"uid":"u1"}}`)
	}))
	defer srv.Close()
	cfg := loginRegion{Name: "cn", BaseURL: srv.URL, StateFile: statePath}
	_, err := pollLogin(srv.Client(), cfg)
	var failure *auth.LoginFailure
	if !errors.As(err, &failure) || !failure.Pending() {
		t.Fatalf("waiting error=%v", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatal("pending state was lost")
	}
	if _, err := pollLogin(srv.Client(), cfg); err != nil {
		t.Fatalf("second poll: %v", err)
	}
}

func TestLoginAPIWillNotFollowRedirects(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	_, status, err := doJSON(source.Client(), loginRegion{}, "GET", source.URL, func(r *http.Request) { r.Header.Set("Authorization", "Bearer test-at") }, nil)
	if err == nil || status != 307 || redirected {
		t.Fatalf("status=%d redirected=%v err=%v", status, redirected, err)
	}
}

// 我们显式声明了 Accept-Encoding，Go 不再自动解压，所以压缩响应必须自己解开。
func TestLoginDecodesCompressedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != loginAcceptEncoding {
			t.Errorf("encoding=%q", r.Header.Get("Accept-Encoding"))
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write([]byte(`{"code":0,"data":{"uid":"u-zip"}}`)); err != nil {
			t.Error(err)
		}
		if err := zw.Close(); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()
	data, status, err := doJSON(srv.Client(), loginRegion{}, http.MethodGet, srv.URL, nil, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if !strings.Contains(string(data), `"u-zip"`) {
		t.Fatalf("data=%s", data)
	}
}

func TestLoginUserAgentFollowsRegionAndEnv(t *testing.T) {
	cn, global := loginRegion{Name: "cn"}, loginRegion{Name: "global"}
	if ua := cn.userAgent(); !strings.HasPrefix(ua, "WorkBuddy/") || !strings.Contains(ua, " CLI/") {
		t.Fatalf("cn ua=%q", ua)
	}
	// 国际版后台服务的两段都是应用名 workbuddy-ai。
	if ua := global.userAgent(); !strings.HasPrefix(ua, "workbuddy-ai/") || strings.Contains(ua, "WorkBuddy/") {
		t.Fatalf("global ua=%q", ua)
	}
	t.Setenv("WB2A_CLIENT_VERSION_CN", "9.9.9")
	t.Setenv("WB2A_CLI_VERSION", "1.2.3")
	if ua := cn.userAgent(); ua != "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/1.2.3" {
		t.Fatalf("env override ua=%q", ua)
	}
}

// 每次登录一条根 span：登录各请求都带同一组根 span 头，traceparent 是每类请求
// 自己的子 span（与根 span 不同、彼此不同），打开授权页另有自己的子 span。
func TestLoginTraceRootAndChildSpans(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	type seen struct {
		path string
		h    http.Header
	}
	var mu sync.Mutex
	var reqs []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, seen{r.URL.Path, r.Header.Clone()})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			io.WriteString(w, `{"code":0,"data":{"state":"s","authUrl":"https://login.example.test/auth"}}`)
		case "/v2/plugin/auth/token":
			io.WriteString(w, `{"code":0,"data":{"accessToken":"at","domain":"www.codebuddy.cn"}}`)
		default:
			io.WriteString(w, `{"code":0,"data":{"uid":"u1"}}`)
		}
	}))
	defer srv.Close()
	cfg := loginRegion{Name: "cn", BaseURL: srv.URL, Origin: "https://login.example.test", StateFile: statePath}
	authURL, err := startLogin(srv.Client(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := readLoginState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pollLogin(srv.Client(), cfg); err != nil {
		t.Fatal(err)
	}

	traceID := strings.ReplaceAll(state.LoginSessionID, "-", "")
	headers := map[string]http.Header{}
	mu.Lock()
	for _, s := range reqs {
		headers[s.path] = s.h
	}
	mu.Unlock()
	// 根 span 的头在每类登录请求上都是同一组，父 span 为空。
	for _, path := range []string{"/v2/plugin/auth/state", "/v2/plugin/auth/token", "/v2/plugin/account"} {
		h := headers[path]
		if h == nil {
			t.Fatalf("missing request %s", path)
		}
		if h.Get("X-Trace-ID") != traceID || h.Get("X-B3-SpanId") != state.RootSpanID {
			t.Errorf("%s root span=%q/%q", path, h.Get("X-Trace-ID"), h.Get("X-B3-SpanId"))
		}
		if h.Get("b3") != traceID+"-"+state.RootSpanID+"-1-" || h.Get("X-B3-ParentSpanId") != "" || h.Get("X-B3-Sampled") != "1" {
			t.Errorf("%s b3 headers=%v", path, h)
		}
	}
	child := func(path string) string {
		parts := strings.Split(headers[path].Get("traceparent"), "-")
		if len(parts) != 4 || parts[0] != "00" || parts[1] != traceID || parts[3] != "01" {
			t.Fatalf("%s traceparent=%q", path, headers[path].Get("traceparent"))
		}
		return parts[2]
	}
	authSpan, tokenSpan, accountSpan := child("/v2/plugin/auth/state"), child("/v2/plugin/auth/token"), child("/v2/plugin/account")
	if authSpan == tokenSpan || tokenSpan == accountSpan || authSpan == accountSpan {
		t.Fatalf("child spans not distinct: %s %s %s", authSpan, tokenSpan, accountSpan)
	}
	if authSpan == state.RootSpanID || tokenSpan == state.RootSpanID || accountSpan == state.RootSpanID {
		t.Fatal("traceparent reused the root span")
	}
	// 打开授权页是另一类请求，用自己的子 span，且不等于发起授权的子 span。
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	pageParts := strings.Split(parsed.Query().Get("traceparent"), "-")
	if len(pageParts) != 4 || pageParts[0] != "00" || pageParts[1] != traceID || pageParts[3] != "01" {
		t.Fatalf("auth page traceparent=%q", parsed.Query().Get("traceparent"))
	}
	if pageParts[2] == authSpan || pageParts[2] == state.RootSpanID {
		t.Fatalf("auth page reused another span: %q", pageParts[2])
	}
}
