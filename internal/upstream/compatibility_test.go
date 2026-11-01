package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestExpertResponseVariants(t *testing.T) {
	for _, tc := range []struct{ data, want string }{
		{`{"items":[{"expert_id":"new"}],"experts":[{"expert_id":"old"}]}`, "new"},
		{`{"items":null,"experts":[{"expert_id":"old"}]}`, "old"},
		{`{"items":[],"experts":[{"expert_id":"old"}]}`, ""},
	} {
		c := testClient(func(*http.Request) (*http.Response, error) {
			return jsonResp(200, `{"code":0,"data":`+tc.data+`}`), nil
		})
		rows, err := c.MarketExpertList(&auth.Auth{AccessToken: "fake"}, "agent")
		if err != nil {
			t.Fatal(err)
		}
		if tc.want == "" {
			if len(rows) != 0 {
				t.Fatal("explicit empty items must win")
			}
		} else if len(rows) != 1 || rows[0].ExpertID != tc.want {
			t.Fatalf("rows=%+v", rows)
		}
	}
}

func TestCatalogueMergesOnlyCurrentEnterprise(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v3/config" {
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"public","maxOutputTokens":1000},{"id":"disabled","disabled":true}]}}`), nil
		}
		enterprise := r.Header.Get("X-Enterprise-Id")
		if r.URL.Path != "/console/enterprises/"+enterprise+"/config/models" {
			t.Fatalf("unexpected enterprise path %s", r.URL.Path)
		}
		return jsonResp(200, fmt.Sprintf(`{"code":0,"data":[{"id":"public","name":"secondary","tags":["chat"]},{"id":%q,"tags":["chat"]},{"id":"disabled","tags":["chat"]},{"id":"image","tags":["text-to-image"]}]}`, enterprise)), nil
	})
	for _, enterprise := range []string{"one", "two"} {
		rows, err := c.FetchModels(&auth.Auth{UID: "same", EnterpriseID: enterprise, AccessToken: "fake"})
		if err != nil || len(rows) != 2 || rows[0].ID != "public" || rows[0].Name != "" || rows[1].ID != enterprise {
			t.Fatalf("enterprise=%s rows=%+v err=%v", enterprise, rows, err)
		}
	}
}

func TestCatalogueFallbackAndEnterpriseFailure(t *testing.T) {
	for _, modernStatus := range []int{404, 401, 429} {
		calls := 0
		c := testClient(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Path == "/v3/config" {
				return jsonResp(modernStatus, `{"code":1}`), nil
			}
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"legacy"}],"agents":[{"name":"cli","models":["legacy"]}]}}`), nil
		})
		rows, err := c.FetchModels(&auth.Auth{AccessToken: "fake"})
		if modernStatus == 404 {
			if err != nil || len(rows) != 1 || calls != 2 {
				t.Fatalf("fallback rows=%v calls=%d err=%v", rows, calls, err)
			}
		} else if err == nil || calls != 1 {
			t.Fatalf("auth/rate failure triggered fallback: calls=%d err=%v", calls, err)
		}
	}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v3/config" {
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"public"}]}}`), nil
		}
		return jsonResp(503, `{}`), nil
	})
	rows, err := c.FetchModels(&auth.Auth{AccessToken: "fake", EnterpriseID: "e"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("enterprise outage lost primary: %v %v", rows, err)
	}
}

func TestPersonalResourcesPreciseAndFreeOnly(t *testing.T) {
	for _, domain := range []string{"", "www.workbuddy.ai"} {
		paidCalls := 0
		c := testClient(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("X-User-Id") != "u" || r.Header.Get("Authorization") != "Bearer fake" {
				t.Fatal("resource request lost account")
			}
			switch r.URL.Path {
			case resourceSummaryAPI:
				return jsonResp(200, `{"code":0,"data":{"Packages":[{"PackageCode":"TCACA_code_001_PqouKr6QWV","CycleTotalCapacity":"2.8","CycleRemainCapacity":"0.4"},{"PackageCode":"TCACA_code_007_nzdH5h4Nl0","CycleTotalCapacity":1.2,"CycleRemainCapacity":"0.4"}],"IsPaidUser":false}}`), nil
			case resourceFreeAPI:
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), "SlicePeriodStartTime") {
					t.Fatal("missing daily slice window")
				}
				return jsonResp(200, `{"code":0,"data":{"Accounts":[],"TotalCount":0}}`), nil
			case resourcePaidAPI:
				paidCalls++
				return jsonResp(200, `{"code":0,"data":{"Accounts":[],"TotalCount":0}}`), nil
			}
			return nil, fmt.Errorf("unexpected path")
		})
		c.ClientSettings = DefaultClientSettings()
		v, err := c.UserResourceDetails(&auth.Auth{UID: "u", AccessToken: "fake", Domain: domain})
		if err != nil || v.Remaining != 1 || v.CapacitySize != 4 || v.RemainingPrecise != "0.800000" {
			t.Fatalf("usage=%+v err=%v", v, err)
		}
		if (domain == "" && paidCalls != 0) || (domain != "" && paidCalls != 1) {
			t.Fatalf("domain=%q paidCalls=%d", domain, paidCalls)
		}
	}
}

func TestResourcePackageFallbackDedupeSlicesAndPlans(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case resourceSummaryAPI:
			return jsonResp(503, `{}`), nil
		case resourceFreeAPI:
			return jsonResp(200, `{"code":0,"data":{"Accounts":[{"ResourceId":"slice","CapacityType":4,"CycleCapacitySizePrecise":"999","CycleCapacityRemainPrecise":"999","SlicePeriodUsageDetails":[{"SlicePeriodCapacitySizePrecise":"2","SlicePeriodCapacityRemainPrecise":"0.4"}]},{"ResourceId":"plan-low","PackageCode":"TCACA_code_002_AkiJS3ZHF5","CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"100"}],"TotalCount":2}}`), nil
		case resourcePaidAPI:
			return jsonResp(200, `{"code":0,"data":{"Accounts":[{"ResourceId":"slice","CycleCapacitySizePrecise":"999","CycleCapacityRemainPrecise":"999"},{"ResourceId":"plan-high","PackageCode":"TCACA_code_027_0FCGVA6vSa","CycleCapacitySizePrecise":"5","CycleCapacityRemainPrecise":"0.4"}],"TotalCount":2}}`), nil
		}
		return nil, fmt.Errorf("unexpected legacy fallback")
	})
	v, err := c.UserResourceDetails(&auth.Auth{AccessToken: "fake"})
	if err != nil || v.CapacitySize != 7 || v.Remaining != 1 || v.RemainingPrecise != "0.800000" {
		t.Fatalf("usage=%+v err=%v", v, err)
	}
}

func TestResourcePaginationAndNoPartialBalance(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == resourceSummaryAPI {
			return jsonResp(200, `{"code":0,"data":{"Packages":[{"PackageCode":"TCACA_code_001_PqouKr6QWV","CycleTotalCapacity":"1","CycleRemainCapacity":"1"}]}}`), nil
		}
		var body struct{ PageNumber int }
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls++
		if body.PageNumber == 1 {
			return jsonResp(200, `{"code":0,"data":{"Accounts":[{"ResourceId":"a"}],"TotalCount":2}}`), nil
		}
		return jsonResp(429, `{"code":1}`), nil
	})
	_, err := c.UserResourceDetails(&auth.Auth{AccessToken: "fake"})
	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) || upstreamErr.Status != 429 || calls != 2 {
		t.Fatalf("partial collection accepted or fallback occurred: calls=%d err=%v", calls, err)
	}
}

func TestReadCancellationAndProxyErrorsNeverFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if legacyReadAllowed(ctx, errReadSchema) || legacyReadAllowed(context.Background(), ErrNoProxyNode) {
		t.Fatal("unexpected fallback")
	}
}

func TestRefreshDiagnosticsKeepClassificationWithoutSecrets(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(401, `{"msg":"SECRET_RESPONSE","code":12153}`), nil
	})
	err := c.RefreshToken(&auth.Auth{AccessToken: "fake", RefreshToken: "fake-r"})
	var upstreamErr *Error
	if err == nil || strings.Contains(err.Error(), "SECRET_RESPONSE") || !strings.Contains(err.Error(), "token request") || !errors.As(err, &upstreamErr) || upstreamErr.Kind != ErrSessionDead {
		t.Fatalf("unsafe or unclassifiable diagnostic: %v", err)
	}
}
