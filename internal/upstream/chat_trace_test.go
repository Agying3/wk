package upstream

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/session"
)

func tracedRequest(ctx context.Context) *http.Request {
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
	applyChatTrace(r)
	return r
}

func TestChatTraceConversationScopeRestartAndFreshAttempt(t *testing.T) {
	doc := map[string]any{"conversation_id": "private-client-session"}
	resolve := func(scope string) string {
		return session.NewResolver([]byte("test-stable-salt")).Resolve(scope, doc, nil, nil).Key
	}
	ctx := WithChatTrace(context.Background(), resolve("tenant-one"))
	first, retry := tracedRequest(ctx), tracedRequest(ctx)
	next := tracedRequest(WithChatTrace(context.Background(), resolve("tenant-one")))
	other := tracedRequest(WithChatTrace(context.Background(), resolve("tenant-two")))
	conversation := first.Header.Get("X-Conversation-ID")
	if conversation == "" || conversation == "private-client-session" || conversation != next.Header.Get("X-Conversation-ID") || conversation == other.Header.Get("X-Conversation-ID") {
		t.Fatal("conversation identity leaked, drifted on resolver restart, or crossed tenant boundary")
	}
	for _, key := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Trace-ID", "X-B3-TraceId"} {
		if first.Header.Get(key) != retry.Header.Get(key) {
			t.Fatalf("retry changed %s", key)
		}
	}
	for _, key := range []string{"X-Request-ID", "X-Conversation-Message-ID", "X-B3-SpanId"} {
		if first.Header.Get(key) == retry.Header.Get(key) {
			t.Fatalf("attempt reused %s", key)
		}
	}
	if first.Header.Get("X-Trace-ID") == next.Header.Get("X-Trace-ID") {
		t.Fatal("independent downstream requests shared one trace")
	}
	trace, span := first.Header.Get("X-Trace-ID"), first.Header.Get("X-B3-SpanId")
	if _, err := hex.DecodeString(trace); err != nil || len(trace) != 32 || len(span) != 16 {
		t.Fatal("malformed trace/span")
	}
	if first.Header.Get("traceparent") != "00-"+trace+"-"+span+"-01" || first.Header.Get("b3") != trace+"-"+span+"-1" {
		t.Fatal("trace formats describe different attempts")
	}
}

func TestChatTraceWithoutIdentityDoesNotMergeRequests(t *testing.T) {
	a := tracedRequest(WithChatTrace(context.Background(), ""))
	b := tracedRequest(WithChatTrace(context.Background(), ""))
	if a.Header.Get("X-Conversation-ID") == b.Header.Get("X-Conversation-ID") {
		t.Fatal("anonymous requests were merged")
	}
	if h := tracedRequest(context.Background()).Header; len(h) != 0 {
		t.Fatal("legacy path gained trace headers")
	}
	if strings.Contains(a.Header.Get("X-Request-ID"), "-") {
		t.Fatal("message ID is not hex")
	}
}
