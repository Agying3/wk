package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	cfg := loginRegion{Origin: "https://www.workbuddy.ai"}
	req := httptest.NewRequest(http.MethodGet, "https://example.test", nil)
	commonHeaders(req, cfg)
	if req.Header.Get("Origin") != "" || req.Header.Get("Referer") != "" || req.Header.Get("User-Agent") != "workbuddy2api-login" {
		t.Fatalf("headers=%v", req.Header)
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
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.URL.Path)
				if r.Header.Get("User-Agent") != clientUA || r.Header.Get("Origin") != "" || r.Header.Get("Referer") != "" || r.Header.Get("X-Refresh-Token") != "" {
					t.Error("unexpected login request headers")
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
			cfg := loginRegion{Name: region, BaseURL: srv.URL, Origin: "https://login.example.test", StateFile: path}
			gotURL, err := startLogin(srv.Client(), cfg)
			if err != nil || gotURL != "https://login.example.test/auth?state=s" {
				t.Fatalf("start: %v", err)
			}
			state, _, err := readLoginState(cfg)
			if err != nil || state.Flow != desktopFlow || state.Region != region {
				t.Fatalf("state=%+v err=%v", state, err)
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
