package upstream

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestChatObserverIsRequestScopedAndCannotBreakTraffic(t *testing.T) {
	count := 0
	ctx := WithChatObserver(context.Background(), func(event ChatObservation) { count++; panic("diagnostic failure") })
	observeChat(ctx, ChatObservation{Phase: "request"})
	observeChat(context.Background(), ChatObservation{Phase: "request"})
	if count != 1 {
		t.Fatal("observer leaked into unrelated context")
	}
}

func TestChatObserverConcurrentReadClose(t *testing.T) {
	ctx := WithChatObserver(context.Background(), func(ChatObservation) {})
	rc := observeChatStream(ctx, io.NopCloser(strings.NewReader(strings.Repeat("data: {\"code\":11140}\n\n", 64))), nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(io.Discard, rc) }()
	go func() {
		defer wg.Done()
		for i := 0; i < 64; i++ {
			_ = rc.Close()
		}
	}()
	wg.Wait()
}

func TestChatObserverSeesUnrecognizedSSEEnvelopeWithoutChangingBytes(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: {\"code\":11128,\"msg\":\"blocked\"}\n\ndata: [DONE]\n\n"
	var events []ChatObservation
	ctx := WithChatObserver(context.Background(), func(e ChatObservation) { events = append(events, e) })
	rc := observeChatStream(ctx, io.NopCloser(strings.NewReader(input)), nil)
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if string(data) != input || len(events) != 1 || events[0].Phase != "stream_error" || BusinessCode(events[0].ErrorBody) != 11128 {
		t.Fatal("SSE observer changed stream or missed root business error")
	}
}
