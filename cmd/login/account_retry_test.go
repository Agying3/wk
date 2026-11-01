package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestPollLoginRetriesAccountNotReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state, _ := json.Marshal(loginState{State: "fixture-state", Region: "cn", Flow: desktopFlow})
	if err := os.WriteFile(path, state, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_LOGIN_STATE_FILE", path)
	accountCalls, tokenCalls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			tokenCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"fixture-token","refreshToken":"fixture-refresh","expiresIn":60,"domain":"www.codebuddy.cn"}}`))
		case "/v2/plugin/account":
			accountCalls++
			if accountCalls == 1 {
				_, _ = w.Write([]byte(`{"code":12151,"data":{}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"fixture-uid","nickname":"fixture-name"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	cfg := loginRegion{Name: "cn", BaseURL: srv.URL, Origin: "https://www.workbuddy.cn", DefaultDomain: "www.codebuddy.cn", StateFile: path}
	result, err := pollLogin(srv.Client(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tokenCalls != 1 || accountCalls != 2 || result["uid"] != "fixture-uid" {
		t.Fatalf("token=%d account=%d result uid=%v", tokenCalls, accountCalls, result["uid"])
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("completed state not removed: %v", err)
	}
}

func TestAccountRetryIsBoundedAndSpecific(t *testing.T) {
	for _, tc := range []struct {
		name                string
		status, code, calls int
	}{
		{"not ready", 200, 12151, 6},
		{"seat full", 200, 12005, 1},
		{"unauthorized", 401, 12151, 1},
		{"server error", 503, 12151, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": tc.code, "data": map[string]any{}})
			}))
			defer srv.Close()
			_, status, err := fetchLoginAccount(srv.Client(), loginRegion{Name: "cn"}, srv.URL, nil, 0)
			var failure *auth.LoginFailure
			if calls != tc.calls || status != tc.status || !errors.As(err, &failure) || failure.Code != tc.code {
				t.Fatalf("calls=%d status=%d err=%v", calls, status, err)
			}
		})
	}
}
