package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// ChatObservation is a synchronous, request-scoped diagnostic hook. Observers
// must not mutate slices/headers or retain credentials from RequestHeaders.
// The hook does not initiate response reads, issue requests, or change retries.
type ChatObservation struct {
	Phase           string
	URL             string
	RequestHeaders  http.Header
	Body            []byte
	Status          int
	ResponseHeaders http.Header
	ErrorBody       []byte
	Err             error
}

// Observe structured upstream SSE errors before the Chat/Responses adapters can
// discard an unfamiliar envelope. Ordinary completion text is never retained.
type observedChatStream struct {
	io.ReadCloser
	mu       sync.Mutex
	ctx      context.Context
	headers  http.Header
	line     []byte
	overlong bool
	errors   int
}

func observeChatStream(ctx context.Context, rc io.ReadCloser, headers http.Header) io.ReadCloser {
	if observer, ok := ctx.Value(chatObserverKey{}).(func(ChatObservation)); !ok || observer == nil {
		return rc
	}
	return &observedChatStream{ReadCloser: rc, ctx: ctx, headers: headers}
}

func (r *observedChatStream) flushLine() {
	defer func() { r.line = r.line[:0]; r.overlong = false }()
	if r.overlong || r.errors >= 8 {
		return
	}
	line := bytes.TrimSpace(r.line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	raw := bytes.TrimSpace(line[5:])
	if !bytes.Contains(raw, []byte(`"code"`)) && !bytes.Contains(raw, []byte(`"error"`)) && !bytes.Contains(raw, []byte(`"type":"error"`)) {
		return
	}
	var event struct {
		Code  json.RawMessage `json:"code"`
		Error json.RawMessage `json:"error"`
		Type  string          `json:"type"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	if len(event.Code) > 0 && !bytes.Equal(event.Code, []byte("0")) && !bytes.Equal(event.Code, []byte("null")) || len(event.Error) > 0 && !bytes.Equal(event.Error, []byte("null")) || event.Type == "error" || event.Type == "response.failed" || event.Type == "response.incomplete" {
		r.errors++
		// The callback may stage this immutable copy until the request finishes.
		observeChat(r.ctx, ChatObservation{Phase: "stream_error", Status: 200, ResponseHeaders: r.headers, ErrorBody: bytes.Clone(raw)})
	}
}

func (r *observedChatStream) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range p[:n] {
		if b == '\n' {
			r.flushLine()
		} else if !r.overlong {
			if len(r.line) < 64<<10 {
				r.line = append(r.line, b)
			} else {
				r.line = nil
				r.overlong = true
			}
		}
	}
	if err != nil && len(r.line) > 0 {
		r.flushLine()
	}
	return n, err
}

func (r *observedChatStream) Close() error {
	r.mu.Lock()
	if len(r.line) > 0 {
		r.flushLine()
	}
	r.mu.Unlock()
	return r.ReadCloser.Close()
}

type chatObserverKey struct{}

func WithChatObserver(ctx context.Context, observe func(ChatObservation)) context.Context {
	return context.WithValue(ctx, chatObserverKey{}, observe)
}

func observeChat(ctx context.Context, event ChatObservation) {
	if observe, ok := ctx.Value(chatObserverKey{}).(func(ChatObservation)); ok && observe != nil {
		// A diagnostic hook must never bring down an ordinary model request.
		defer func() { _ = recover() }()
		observe(event)
	}
}
