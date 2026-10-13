package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// This audit compares the complete HTTP handler with an independent reading
// of the upstream usage snapshots. It does not use the production usage
// merger or stats helpers as its expected-value oracle.
type auditCounts struct {
	Input      int  `json:"input"`
	Output     int  `json:"output"`
	Cache      int  `json:"cache"`
	CacheKnown bool `json:"cache_known"`
}

type auditMode struct {
	name, path  string
	stream, raw bool
}

var auditModes = []auditMode{
	{"chat_stream", "/v1/chat/completions", true, false},
	{"chat_json", "/v1/chat/completions", false, false},
	{"responses_stream", "/v1/responses", true, false},
	{"responses_json", "/v1/responses", false, false},
	{"chat_raw", "/v1/chat/completions", true, true},
}

func auditUsageFrames(data []byte, stream, responses bool) ([]map[string]any, error) {
	var frames []map[string]any
	decode := func(raw []byte) error {
		var event map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&event); err != nil {
			return err
		}
		if event["error"] != nil || event["type"] == "response.failed" {
			return fmt.Errorf("response has an error event")
		}
		if responses {
			if r, ok := event["response"].(map[string]any); ok {
				event = r
			}
		}
		if u, ok := event["usage"].(map[string]any); ok {
			frames = append(frames, u)
		}
		return nil
	}
	if !stream {
		if err := decode(data); err != nil {
			return nil, err
		}
		return frames, nil
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		if err := decode(payload); err != nil {
			return nil, err
		}
	}
	return frames, nil
}

func auditNumber(m map[string]any, path string) (int, bool) {
	parts := strings.Split(path, ".")
	var value any = m
	for _, p := range parts {
		obj, ok := value.(map[string]any)
		if !ok {
			return 0, false
		}
		value, ok = obj[p]
		if !ok || value == nil {
			return 0, false
		}
	}
	switch n := value.(type) {
	case json.Number:
		v, err := n.Int64()
		return int(v), err == nil
	case float64:
		return int(n), n >= 0 && n == float64(int(n))
	}
	return 0, false
}

func auditSnapshotCounts(frames []map[string]any) auditCounts {
	var c auditCounts
	for _, u := range frames {
		groups := [][]string{
			{"prompt_tokens", "input_tokens"},
			{"completion_tokens", "output_tokens"},
			{"prompt_cache_hit_tokens", "prompt_tokens_details.cached_tokens", "input_tokens_details.cached_tokens", "cache_read_input_tokens"},
		}
		for group, paths := range groups {
			for _, p := range paths {
				if n, ok := auditNumber(u, p); ok {
					switch group {
					case 0:
						c.Input = n
					case 1:
						c.Output = n
					case 2:
						c.Cache, c.CacheKnown = n, true
					}
					break
				}
			}
		}
	}
	return c
}

// Limit saved live evidence to numeric usage fields. No request content,
// credentials, IDs, or unrecognized provider metadata are emitted.
func auditNumericUsage(frames []map[string]any) []map[string]any {
	var filter func(map[string]any) map[string]any
	filter = func(m map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			switch n := v.(type) {
			case json.Number, float64:
				out[k] = n
			case map[string]any:
				if nested := filter(n); len(nested) > 0 {
					out[k] = nested
				}
			}
		}
		return out
	}
	out := make([]map[string]any, 0, len(frames))
	for _, frame := range frames {
		out = append(out, filter(frame))
	}
	return out
}

func auditBody(mode auditMode, model, text string) []byte {
	messages := []any{map[string]any{"role": "system", "content": "Synthetic API usage audit. Reply only OK; no explanation."}, map[string]any{"role": "user", "content": text}}
	doc := map[string]any{"model": model, "stream": mode.stream, "reasoning_effort": "low"}
	if mode.path == "/v1/responses" {
		doc["input"], doc["max_output_tokens"] = messages, 32
	} else {
		doc["messages"], doc["max_tokens"] = messages, 32
		doc["stream_options"] = map[string]any{"include_usage": true}
	}
	raw, _ := json.Marshal(doc)
	return raw
}

type auditChunkReader struct {
	reader io.Reader
	size   int
}

func (r auditChunkReader) Read(p []byte) (int, error) {
	if len(p) > r.size {
		p = p[:r.size]
	}
	return r.reader.Read(p)
}

func auditAssertReturn(t *testing.T, mode auditMode, rec *httptest.ResponseRecorder, store *captureRequestLogStore, source []map[string]any, want auditCounts) []map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP status=%d", rec.Code)
	}
	returned, err := auditUsageFrames(rec.Body.Bytes(), mode.stream, mode.path == "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	if got := auditSnapshotCounts(returned); got != want {
		t.Errorf("API usage=%+v; upstream=%+v", got, want)
	}
	if len(store.records) != 1 {
		t.Fatalf("internal records=%d", len(store.records))
	}
	r := store.records[0]
	if r.Status != 200 || int(r.InputTokens) != want.Input || int(r.OutputTokens) != want.Output || int(r.CacheReadTokens) != want.Cache {
		t.Errorf("internal usage=%d/%d/%d status=%d; upstream=%+v", r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.Status, want)
	}
	if mode.path == "/v1/chat/completions" && mode.stream && !reflect.DeepEqual(returned, source) {
		t.Error("Chat SSE usage fields changed during forwarding")
	}
	return returned
}

var usageReturnAuditCases = []struct {
	name      string
	snapshots []string
	want      auditCounts
}{
	{"native_aliases", []string{`{"prompt_tokens":17686,"completion_tokens":612,"prompt_cache_hit_tokens":7680,"cache_read_input_tokens":0,"cached_tokens":0,"prompt_tokens_details":{"cached_tokens":7680}}`}, auditCounts{17686, 612, 7680, true}},
	{"partial_tail", []string{`{"prompt_tokens":18704,"prompt_cache_hit_tokens":17920}`, `{"completion_tokens":12}`, `{"credit":0.1}`, `{}`}, auditCounts{18704, 12, 17920, true}},
	{"explicit_zero", []string{`{"prompt_tokens":18704,"completion_tokens":12,"prompt_cache_hit_tokens":17920}`, `{"input_tokens_details":{"cached_tokens":0}}`, `{}`}, auditCounts{18704, 12, 0, true}},
	{"smaller_final_cache", []string{`{"prompt_tokens":18951,"completion_tokens":8,"prompt_cache_hit_tokens":17920}`, `{"prompt_cache_hit_tokens":7552}`}, auditCounts{18951, 8, 7552, true}},
	{"nested_prompt", []string{`{"prompt_tokens":18984,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":18944}}`}, auditCounts{18984, 8, 18944, true}},
	{"nested_input", []string{`{"input_tokens":18984,"output_tokens":8,"input_tokens_details":{"cached_tokens":18944}}`}, auditCounts{18984, 8, 18944, true}},
	{"flat_anthropic", []string{`{"input_tokens":12798,"output_tokens":161,"cache_read_input_tokens":12544}`}, auditCounts{12798, 161, 12544, true}},
	{"missing_cache", []string{`{"prompt_tokens":1000,"completion_tokens":8}`, `{"credit":0}`, `{}`}, auditCounts{1000, 8, 0, false}},
}

func TestUsageReturnAudit(t *testing.T) {
	for _, tc := range usageReturnAuditCases {
		for _, mode := range auditModes {
			for _, size := range []int{1, 7, 65536} {
				t.Run(fmt.Sprintf("%s/%s/chunk_%d", tc.name, mode.name, size), func(t *testing.T) {
					sse := "data: {\"id\":\"audit-fixture\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n"
					for _, u := range tc.snapshots {
						sse += "data: {\"id\":\"audit-fixture\",\"choices\":[],\"usage\":" + u + "}\n\n"
					}
					sse += "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
					source, err := auditUsageFrames([]byte(sse), true, false)
					if err != nil {
						t.Fatal(err)
					}
					if got := auditSnapshotCounts(source); got != tc.want {
						t.Fatalf("invalid fixture oracle=%+v", got)
					}
					up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sse, true })
					base := up.HTTP.Transport
					up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
						resp, err := base.RoundTrip(r)
						if err == nil {
							resp.Body = io.NopCloser(auditChunkReader{reader: resp.Body, size: size})
						}
						return resp, err
					})
					store := &captureRequestLogStore{}
					h := newTestHandler(t, Config{Pool: testPoolWith(&auth.Auth{UID: "audit", AccessToken: "fixture-only", ExpiresAt: 9999999999}), Upstream: up, RequestLogStore: store, Passthrough: mode.raw})
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, mode.path, bytes.NewReader(auditBody(mode, "glm-5.3-flash", "Return OK."))))
					auditAssertReturn(t, mode, rec, store, source, tc.want)
					if mode.raw && rec.Body.String() != sse {
						t.Error("raw SSE bytes changed")
					}
				})
			}
		}
	}
}

type auditCapturedBody struct {
	io.ReadCloser
	captured *bytes.Buffer
}

func (b *auditCapturedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.captured.Len()+n > 2<<20 {
		return 0, fmt.Errorf("audit response exceeded limit")
	}
	b.captured.Write(p[:n])
	return n, err
}

// Explicitly opt-in only. A server-side managed credential is read in memory;
// it is never emitted, copied, refreshed, or written back. No listener is opened.
func TestLiveUsageReturnAudit(t *testing.T) {
	file := os.Getenv("WB_USAGE_AUDIT_AUTH_FILE")
	if file == "" {
		t.Skip("live audit requires explicit opt-in")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("could not read audit credential")
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatal("could not parse audit credential")
	}
	if a.NeedsRefresh(10 * time.Minute) {
		t.Fatal("audit credential is near expiry; no refresh performed")
	}
	accountHash := sha256.Sum256([]byte(a.UID))
	accountDigest := hex.EncodeToString(accountHash[:])[:12]
	up := upstream.New()
	base := up.HTTP.Transport
	var captured bytes.Buffer
	var sentHash string
	calls := 0
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v2/chat/completions" {
			return nil, fmt.Errorf("audit forbids non-chat calls")
		}
		if r.GetBody != nil {
			body, _ := r.GetBody()
			b, _ := io.ReadAll(body)
			body.Close()
			sum := sha256.Sum256(b)
			sentHash = hex.EncodeToString(sum[:])
		}
		calls++
		resp, err := base.RoundTrip(r)
		if err == nil {
			resp.Body = &auditCapturedBody{ReadCloser: resp.Body, captured: &captured}
		}
		return resp, err
	})
	p := testPoolWith(a)
	p.SetMaxInFlight(1)
	store := &captureRequestLogStore{}
	h := newTestHandler(t, Config{Pool: p, Upstream: up, RequestLogStore: store, MaxRotate: 1})
	seed := "Synthetic usage audit " + time.Now().UTC().Format(time.RFC3339Nano) + "\n"
	text := seed
	for i := 0; i < 650; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s%d", seed, i)))
		text += fmt.Sprintf("record %04d value %x status active\n", i, sum[:16])
	}
	text += "\nReply only OK."
	for _, model := range []string{"glm-5.3-flash", "deepseek-v4.1-flash"} {
		// Four streaming repetitions plus JSON and Responses exercise exactly
		// the same upstream parser/HTTP handler, without a production restart.
		modes := []auditMode{auditModes[0], auditModes[0], auditModes[0], auditModes[0], auditModes[1], auditModes[2]}
		for index, mode := range modes {
			t.Run(fmt.Sprintf("%s/%d_%s", model, index, mode.name), func(t *testing.T) {
				captured.Reset()
				store.records = nil
				calls = 0
				started := time.Now()
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, mode.path, bytes.NewReader(auditBody(mode, model, text))))
				source, err := auditUsageFrames(captured.Bytes(), true, false)
				if err != nil {
					t.Fatal(err)
				}
				if len(source) == 0 {
					t.Fatalf("no raw upstream usage: API status=%d", rec.Code)
				}
				want := auditSnapshotCounts(source)
				if !want.CacheKnown || want.Input <= 0 {
					t.Fatal("raw usage is incomplete")
				}
				returned := auditAssertReturn(t, mode, rec, store, source, want)
				if calls != 1 {
					t.Errorf("upstream requests=%d", calls)
				}
				row := map[string]any{"model": model, "index": index, "mode": mode.name, "upstream_requests": calls, "http_status": rec.Code, "upstream_counts": want, "api_counts": auditSnapshotCounts(returned), "raw_usage_frames": auditNumericUsage(source), "api_usage_frames": auditNumericUsage(returned), "account_digest": accountDigest, "outbound_body_sha256": sentHash, "elapsed_ms": time.Since(started).Milliseconds(), "passed": !t.Failed()}
				if len(store.records) == 1 {
					r := store.records[0]
					row["internal_counts"] = map[string]int64{"input": r.InputTokens, "output": r.OutputTokens, "cache": r.CacheReadTokens}
				}
				b, _ := json.Marshal(row)
				fmt.Printf("WB_USAGE_AUDIT %s\n", b)
			})
			if t.Failed() {
				return
			}
		}
	}
}
