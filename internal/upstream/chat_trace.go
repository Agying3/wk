package upstream

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
)

type chatTraceKey struct{}
type chatTrace struct {
	conversation string
	request      string
}

// WithChatTrace takes only the already scoped, HMAC-protected Identity.Key.
// It must be called once before account rotation, never with raw client IDs,
// account UIDs or rewritten prompt text. No new affinity store is needed:
// stable identity and its existing salt determine the conversation ID.
func WithChatTrace(ctx context.Context, identityKey string) context.Context {
	conversation := NewEventID()
	if identityKey != "" {
		sum := sha256.Sum256([]byte("outbound-conversation-v1\x00" + identityKey))
		// HMAC-derived identity keys are opaque. Preserve the UUID wire shape
		// while deriving a stable value independent of the chosen account.
		sum[6] = sum[6]&0x0f | 0x40
		sum[8] = sum[8]&0x3f | 0x80
		conversation = fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
	}
	return context.WithValue(ctx, chatTraceKey{}, chatTrace{conversation: conversation, request: traceHex()})
}

func traceHex() string { return strings.ReplaceAll(NewEventID(), "-", "") }

func applyChatTrace(req *http.Request) {
	trace, ok := req.Context().Value(chatTraceKey{}).(chatTrace)
	if !ok {
		return
	}
	message := traceHex()
	span := traceHex()[:16]
	req.Header.Set("X-Conversation-ID", trace.conversation)
	req.Header.Set("X-Conversation-Request-ID", trace.request)
	req.Header.Set("X-Conversation-Message-ID", message)
	req.Header.Set("X-Request-ID", message)
	req.Header.Set("X-Trace-ID", trace.request)
	req.Header.Set("X-B3-TraceId", trace.request)
	req.Header.Set("X-B3-SpanId", span)
	req.Header.Set("X-B3-Sampled", "1")
	req.Header.Set("b3", trace.request+"-"+span+"-1")
	req.Header.Set("traceparent", "00-"+trace.request+"-"+span+"-01")
}
