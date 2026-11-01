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

// Accept-Encoding 按发请求的运行时分别声明。官方在哪个请求上都没有改写这个头：
// 内置 CLI 与后台服务走 axios（它的 http 适配器补 "gzip, compress, deflate" 加 br，
// zstd 开关默认关），网页登录码走 Node 内置 fetch（undici 对 https 补
// "br, gzip, deflate"），官网网页是桌面 Chrome（"gzip, deflate, br, zstd"），
// 小程序 web-view 是 iOS WebKit（"gzip, deflate, br"）。
//
// 声明必须与这里的解码能力一致：解不开的编码不声明，也不要为了像另一个运行时而声明。
const (
	acceptEncodingAxios     = "gzip, compress, deflate, br"
	acceptEncodingNodeFetch = "br, gzip, deflate"
	acceptEncodingChrome    = "gzip, deflate, br, zstd"
	acceptEncodingWebKit    = "gzip, deflate, br"
)

// applyEncoding 显式声明本请求的 Accept-Encoding。doUpstream 只在头为空时补默认值，
// 所以调用点先设就能按请求类别区分。
func applyEncoding(req *http.Request, value string) {
	req.Header.Set("Accept-Encoding", value)
}

// AcceptEncodingAxios 供独立程序（如登录辅助程序）复用同一组声明，避免各写一份漂移。
const AcceptEncodingAxios = acceptEncodingAxios

// doUpstream advertises only encodings we can decode. Decode after Do, so the
// existing account proxy, transport, deadlines and cancellation remain in use.
func doUpstream(client *http.Client, req *http.Request) (*http.Response, error) {
	if req.Header.Get("Accept-Encoding") == "" && req.Method != http.MethodHead && req.Header.Get("Range") == "" {
		req = req.Clone(req.Context())
		// 没指定类别的出站请求按 API 请求（axios）声明。
		req.Header.Set("Accept-Encoding", acceptEncodingAxios)
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
		case "gzip", "x-gzip", "compress", "x-compress", "deflate", "br", "zstd", "identity":
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

// DecodeResponse 按 Content-Encoding 给响应体套上惰性解压，供独立程序（如登录辅助
// 程序）复用同一套解码规则。不需要解压或不认识的编码返回 ok=false，调用方照用 raw。
func DecodeResponse(contentEncoding string, raw io.ReadCloser) (io.ReadCloser, bool) {
	codec := strings.ToLower(strings.TrimSpace(contentEncoding))
	switch codec {
	case "", "identity":
		return raw, false
	case "gzip", "x-gzip", "compress", "x-compress", "deflate", "br", "zstd":
		return &decodedBody{raw: raw, codec: codec}, true
	default:
		return raw, false
	}
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
	case "compress", "x-compress":
		// axios 解 compress 用的是 zlib.createUnzip：按头部自动分辨 gzip 与 zlib。
		raw := bufio.NewReader(b.raw)
		header, err := raw.Peek(2)
		if err != nil {
			b.openErr = err
			return
		}
		var r io.ReadCloser
		if header[0] == 0x1f && header[1] == 0x8b {
			r, err = gzip.NewReader(raw)
		} else {
			r, err = zlib.NewReader(raw)
		}
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
