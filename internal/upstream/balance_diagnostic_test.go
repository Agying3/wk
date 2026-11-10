package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

type balanceDiagnosticTransport func(*http.Request) (*http.Response, error)

func (f balanceDiagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func balanceDiagnosticClient(t *testing.T, reply func(string) (int, string)) (*Client, *[]string, *bytes.Buffer) {
	t.Helper()
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(old) })
	paths := []string{}
	c := New()
	c.HTTP.Transport = balanceDiagnosticTransport(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		status, body := reply(r.URL.Path)
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	return c, &paths, &output
}

func loggedBalanceTrace(t *testing.T, output *bytes.Buffer) balanceTrace {
	t.Helper()
	_, tail, ok := strings.Cut(output.String(), " trace=")
	if !ok || strings.Count(output.String(), "balance_diagnostic ") != 1 {
		t.Fatalf("expected exactly one diagnostic: %s", output)
	}
	var trace balanceTrace
	if err := json.Unmarshal([]byte(strings.TrimSpace(tail)), &trace); err != nil {
		t.Fatal(err)
	}
	return trace
}

func TestBalanceDiagnosticKeepsPrimaryAndLegacyFailureWithoutSecrets(t *testing.T) {
	c, paths, output := balanceDiagnosticClient(t, func(path string) (int, string) {
		switch path {
		case resourceSummaryAPI:
			return 200, `{"code":0,"data":{"Packages":[]},"accessToken":"token-secret"}`
		case resourceFreeAPI:
			return 200, `{"code":0,"data":{"Accounts":[{"CycleCapacitySizePrecise":"","PackageCode":"package-secret","ResourceId":"id-secret"}],"TotalCount":1},"msg":"message-secret"}`
		default:
			return 200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CapacitySize":"type-secret"}]}}}}`
		}
	})
	_, err := c.UserResourceDetailsContext(context.Background(), &auth.Auth{UID: "uid-secret", AccessToken: "credential-secret"})
	var typed *json.UnmarshalTypeError
	if !errors.As(err, &typed) || len(*paths) != 3 || (*paths)[2] != "/v2/billing/meter/get-user-resource" {
		t.Fatalf("fallback behavior changed: paths=%v err=%v", paths, err)
	}
	trace := loggedBalanceTrace(t, output)
	if len(trace.Reads) != 3 || trace.Reads[1].Schema["$.data.Accounts[0].CycleCapacitySizePrecise"] != "string_empty" {
		t.Fatalf("missing first failure shape: %+v", trace)
	}
	primary, legacy := false, false
	for _, f := range trace.Failures {
		primary = primary || f.Stage == "free" && f.Reason == "decode"
		legacy = legacy || f.Stage == "legacy" && f.Kind == "json_type" && f.Field == "Response.Data.Accounts.CapacitySize"
	}
	if !primary || !legacy {
		t.Fatalf("lost primary or fallback failure: %+v", trace.Failures)
	}
	for _, secret := range []string{"token-secret", "package-secret", "id-secret", "message-secret", "type-secret", "uid-secret", "credential-secret", "accessToken", "ResourceId"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("diagnostic leaked %q", secret)
		}
	}
}

func TestBalanceDiagnosticForbiddenPreservesClassificationAndCallCount(t *testing.T) {
	c, paths, output := balanceDiagnosticClient(t, func(string) (int, string) {
		return 403, `{"code":11140,"msg":"echo-secret","data":{"token":"data-secret"}}`
	})
	_, err := c.UserResourceDetailsContext(context.Background(), &auth.Auth{UID: "fixture"})
	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) || upstreamErr.Kind != ErrRiskFlag || len(*paths) != 1 {
		t.Fatalf("classification or request count changed: %v %v", paths, err)
	}
	trace := loggedBalanceTrace(t, output)
	if len(trace.Reads) != 1 || trace.Reads[0].HTTP != 403 || trace.Reads[0].Code == nil || *trace.Reads[0].Code != 11140 {
		t.Fatalf("missing status/code: %+v", trace)
	}
	if strings.Contains(output.String(), "echo-secret") || strings.Contains(output.String(), "data-secret") {
		t.Fatal("upstream error text leaked")
	}
}

func TestBalanceDiagnosticSuccessIsSilent(t *testing.T) {
	c, paths, output := balanceDiagnosticClient(t, func(path string) (int, string) {
		if path == resourceSummaryAPI {
			return 200, `{"code":0,"data":{"Packages":[{"PackageCode":"TCACA_code_001_PqouKr6QWV","CycleTotalCapacity":"100","CycleRemainCapacity":"80"}],"IsPaidUser":false}}`
		}
		return 200, `{"code":0,"data":{"Accounts":[],"TotalCount":0}}`
	})
	usage, err := c.UserResourceDetailsContext(context.Background(), &auth.Auth{UID: "fixture"})
	if err != nil || usage.Remaining != 80 || len(*paths) != 2 || output.Len() != 0 {
		t.Fatalf("healthy balance changed or logged: usage=%+v paths=%v err=%v log=%s", usage, paths, err, output)
	}
}

func TestBalanceDiagnosticSuccessfulFallbackIsSilent(t *testing.T) {
	c, paths, output := balanceDiagnosticClient(t, func(path string) (int, string) {
		if path == "/v2/billing/meter/get-user-resource" {
			return 200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CapacityRemain":50}]}}}}`
		}
		return 200, `{"code":0,"data":{"Accounts":null}}`
	})
	usage, err := c.UserResourceDetailsContext(context.Background(), &auth.Auth{UID: "fixture"})
	if err != nil || usage.Remaining != 50 || len(*paths) != 3 || output.Len() != 0 {
		t.Fatalf("successful fallback changed: paths=%v err=%v log=%s", paths, err, output)
	}
}

func TestBalanceDiagnosticCanceledDoesNotFallback(t *testing.T) {
	c, paths, output := balanceDiagnosticClient(t, func(string) (int, string) { return 200, `{}` })
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.HTTP.Transport = balanceDiagnosticTransport(func(*http.Request) (*http.Response, error) {
		calls++
		cancel()
		return nil, context.Canceled
	})
	_, err := c.UserResourceDetailsContext(ctx, &auth.Auth{UID: "fixture"})
	if !errors.Is(err, context.Canceled) || calls != 1 || len(*paths) != 0 {
		t.Fatalf("cancellation semantics changed: calls=%d err=%v", calls, err)
	}
	trace := loggedBalanceTrace(t, output)
	if trace.Failures[len(trace.Failures)-1].Kind != "canceled" {
		t.Fatal("cancellation category lost")
	}
}

func TestBalanceDiagnosticLimitsAndUntrustedFieldNames(t *testing.T) {
	trace := &balanceTrace{}
	ctx := context.WithValue(context.Background(), balanceTraceKey{}, trace)
	r, _ := http.NewRequestWithContext(ctx, "POST", "https://example.invalid"+resourceFreeAPI, nil)
	for i := 0; i < 30; i++ {
		observeBalanceRead(r, &http.Response{StatusCode: 200}, []byte(strings.Repeat("x", (1<<20)+1)), nil)
	}
	if len(trace.Reads) != 24 || !trace.Truncated || !trace.Reads[0].SchemaLimit || len(trace.Reads[0].Schema) != 0 {
		t.Fatal("trace/parse limits not enforced")
	}
	var parsed struct{ CapacitySize int }
	err := json.Unmarshal([]byte(`{"CapacitySize":"value-secret"}`), &parsed)
	var te *json.UnmarshalTypeError
	if !errors.As(err, &te) {
		t.Fatal("fixture did not produce type error")
	}
	te.Field = "untrusted-secret.CapacitySize"
	noteBalanceFailure(ctx, "free", "decode", te)
	if trace.Failures[0].Field != "" {
		t.Fatal("untrusted error field name leaked")
	}
}
