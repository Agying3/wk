package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestAccountModelCapabilitiesAreIsolated(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) { return nil, errors.New("not used") })
	accounts := []*auth.Auth{{UID: "u", Domain: "www.codebuddy.cn"}, {UID: "u", Domain: "www.workbuddy.ai"}, {UID: "u", EnterpriseID: "one"}, {UID: "u", EnterpriseID: "two"}, {UID: "other"}, {AccessToken: "fake-anonymous-one"}, {AccessToken: "fake-anonymous-two"}}
	for i, a := range accounts {
		values := []string{"low"}
		if i%2 != 0 {
			values = []string{"high"}
		}
		c.storeCapabilities(a, map[string][]string{"m": values})
	}
	raw := []byte(`{"messages":[{"role":"user","content":"unchanged"}],"model":"m","reasoning_effort":"max"}`)
	var wg sync.WaitGroup
	for i, a := range accounts {
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			for range 20 {
				out, err := c.accountEffortBody(a, raw)
				if err != nil {
					t.Error(err)
					return
				}
				var obj map[string]any
				_ = json.Unmarshal(out, &obj)
				want := "low"
				if i%2 != 0 {
					want = "high"
				}
				if obj["reasoning_effort"] != want {
					t.Errorf("scope=%d effort=%v", i, obj["reasoning_effort"])
				}
			}
		}(i, a)
	}
	wg.Wait()
	if out, err := c.accountEffortBody(&auth.Auth{UID: "unknown"}, raw); err != nil || !bytes.Equal(out, raw) {
		t.Fatal("unknown account borrowed another capability")
	}
	prepared, err := c.PrepareChatBody(map[string]any{"model": "m", "reasoning_effort": "max", "messages": []any{}})
	if err != nil || !strings.Contains(string(prepared), `"reasoning_effort":"max"`) {
		t.Fatal("pre-account preparation changed effort")
	}
	c.storeCapabilities(accounts[0], nil)
	if out, _ := c.accountEffortBody(accounts[0], raw); !bytes.Equal(out, raw) {
		t.Fatal("missing declaration retained stale capability")
	}
	if string(raw) != `{"messages":[{"role":"user","content":"unchanged"}],"model":"m","reasoning_effort":"max"}` {
		t.Fatal("shared body mutated")
	}
}

func TestBusinessCodeExtractionAndExistingPolicy(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		code int
	}{
		{`{"code":"14003"}`, 14003}, {`{"code":400,"error":{"code":11158}}`, 11158}, {`{"response":{"data":{"code":"6004"}}}`, 6004}, {`{"data":{"code":77777}}`, 77777}, {`{"code":1.5}`, 0}, {`{"code":"bad"}`, 0}, {`{"code":0}`, 0},
	} {
		if got := BusinessCode([]byte(tc.raw)); got != tc.code {
			t.Errorf("got=%d want=%d", got, tc.code)
		}
	}
	for _, tc := range []struct {
		body string
		kind ErrKind
		code int
	}{
		{`{"code":"6004","msg":""}`, ErrSoftRate, 6004}, {`{"code":"14018","msg":""}`, ErrHardCredit, 14018}, {`{"code":"77777","msg":"unknown"}`, ErrClient, 77777},
	} {
		c := testClient(func(*http.Request) (*http.Response, error) { return jsonResp(200, tc.body), nil })
		req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://local.example", nil)
		_, err := c.doJSON(req, &auth.Auth{})
		var e *Error
		if !errors.As(err, &e) || e.Kind != tc.kind || e.Code != tc.code {
			t.Fatalf("kind/code mismatch: %v", err)
		}
	}
}

func TestReasoningSummaryRespectsCaller(t *testing.T) {
	c := testClient(nil)
	c.ReasoningSummaryAuto = true
	for _, tc := range []struct {
		body string
		want string
	}{
		{`{"reasoning_effort":"high"}`, `"reasoning_summary":"auto"`}, {`{"reasoning":{"effort":"high"}}`, `"reasoning_summary":"auto"`}, {`{"reasoning_effort":"high","reasoning_summary":null}`, `"reasoning_summary":null`},
	} {
		out, err := c.prepareBody([]byte(tc.body))
		if err != nil || !strings.Contains(string(out), tc.want) || strings.Contains(string(out), "cache_retention") {
			t.Fatalf("body=%s err=%v", out, err)
		}
	}
	out, _ := c.prepareBody([]byte(`{"model":"m"}`))
	if strings.Contains(string(out), "reasoning_summary") {
		t.Fatal("summary added without effort")
	}
	c.ReasoningSummaryAuto = false
	out, _ = c.prepareBody([]byte(`{"reasoning_effort":"high"}`))
	if strings.Contains(string(out), "reasoning_summary") {
		t.Fatal("summary opt-out ignored")
	}
}

func TestExpertRequestUsesActualAttributes(t *testing.T) {
	for _, tc := range []struct {
		expert          string
		team, protected bool
	}{
		{`{"expert_id":"real","expert_type":"team","is_cloud":1}`, true, true}, {`{"expert_id":"real","need_pay":1,"pay_detail":{"pay_mode":0}}`, false, false}, {`{"expert_id":"real","pay_detail":{"payMode":2}}`, false, true}, {`{"expert_id":"real"}`, false, false},
	} {
		var expert MarketExpert
		_ = json.Unmarshal([]byte(tc.expert), &expert)
		c := testClient(func(r *http.Request) (*http.Response, error) {
			if (r.Header.Get("X-Expert-Team-Task") == "true") != tc.team || (r.Header.Get("X-Moderation-Type") != "") != tc.protected {
				t.Error("incorrect expert flags")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"id\":\"cmb-0123456789abcdef0123456789abcdef\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))}, nil
		})
		if _, err := c.DesktopChatWithExpertInfoObserved(&auth.Auth{UID: "u", AccessToken: "fake"}, expert); err != nil {
			t.Fatal(err)
		}
	}
}
