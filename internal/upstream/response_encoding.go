package upstream

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// doUpstream advertises only encodings we can decode. Decode after Do, so the
// existing account proxy, transport, deadlines and cancellation remain in use.
func doUpstream(client *http.Client, req *http.Request) (*http.Response, error) {
	if req.Header.Get("Accept-Encoding") == "" && req.Method != http.MethodHead && req.Header.Get("Range") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	}
	resp, err := client.Do(req)
	if err != nil || resp.Body == nil || resp.Uncompressed {
		return resp, err
	}
	encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return resp, nil
	}
	parts := strings.Split(strings.ToLower(encoding), ",")
	if len(parts) > 4 {
		resp.Body.Close()
		return nil, fmt.Errorf("response has too many content encodings")
	}
	for _, part := range parts {
		switch strings.TrimSpace(part) {
		case "gzip", "x-gzip", "deflate", "br", "zstd", "identity":
		default:
			resp.Body.Close()
			return nil, fmt.Errorf("unsupported response content encoding")
		}
	}
	for i := len(parts) - 1; i >= 0; i-- {
		if codec := strings.TrimSpace(parts[i]); codec != "identity" {
			resp.Body = &decodedBody{raw: resp.Body, codec: codec}
		}
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp, nil
}

// Opening a gzip/zstd reader may consume bytes. Keep that work inside Read,
// after the SSE idle/total timeout wrapper has taken ownership of the body.
type decodedBody struct {
	raw       io.ReadCloser
	codec     string
	mu        sync.Mutex
	reader    io.Reader
	cleanup   func()
	openErr   error
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func (b *decodedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if b.reader == nil && b.openErr == nil {
		b.open()
	}
	if b.openErr != nil {
		return 0, b.openErr
	}
	return b.reader.Read(p)
}

func (b *decodedBody) open() {
	switch b.codec {
	case "gzip", "x-gzip":
		r, err := gzip.NewReader(b.raw)
		b.openErr = err
		if err == nil {
			b.reader, b.cleanup = r, func() { _ = r.Close() }
		}
	case "deflate":
		raw := bufio.NewReader(b.raw)
		header, err := raw.Peek(2)
		if err != nil {
			b.openErr = err
			return
		}
		var r io.ReadCloser
		// RFC 1950 CMF/FLG distinguish a zlib wrapper from raw DEFLATE.
		if header[0]&15 == 8 && header[0]>>4 <= 7 && (int(header[0])*256+int(header[1]))%31 == 0 {
			r, err = zlib.NewReader(raw)
		} else {
			r = flate.NewReader(raw)
		}
		b.openErr = err
		if err == nil {
			b.reader, b.cleanup = r, func() { _ = r.Close() }
		}
	case "br":
		b.reader = brotli.NewReader(b.raw)
	case "zstd":
		r, err := zstd.NewReader(b.raw, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
		b.openErr = err
		if err == nil {
			b.reader, b.cleanup = r, r.Close
		}
	}
}

func (b *decodedBody) Close() error {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		// Closing the network body first interrupts a blocked Read/open.
		b.closeErr = b.raw.Close()
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.cleanup != nil {
			b.cleanup()
		}
	})
	return b.closeErr
}

func readControlBody(body io.Reader) ([]byte, error) {
	const limit = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read upstream response: %w", err)
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("upstream response exceeds decoded size limit")
	}
	return raw, nil
}
