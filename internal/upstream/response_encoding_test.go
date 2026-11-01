package upstream

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

	"workbuddy2api/internal/auth"
)

func compressFixture(t *testing.T, codec string, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	var writer io.WriteCloser
	switch codec {
	case "gzip":
		writer = gzip.NewWriter(&out)
	case "deflate":
		writer = zlib.NewWriter(&out)
	case "raw-deflate":
		var err error
		writer, err = flate.NewWriter(&out, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
	case "br":
		writer = brotli.NewWriter(&out)
	case "zstd":
		var err error
		writer, err = zstd.NewWriter(&out, zstd.WithEncoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestResponseEncodingsAndDecodedLimit(t *testing.T) {
	for _, codec := range []string{"gzip", "deflate", "raw-deflate", "br", "zstd", "gzip, br"} {
		t.Run(codec, func(t *testing.T) {
			plain := []byte(`{"code":0,"data":{"ok":true}}`)
			encoding := codec
			var compressed []byte
			if codec == "gzip, br" {
				compressed = compressFixture(t, "br", compressFixture(t, "gzip", plain))
			} else {
				compressed = compressFixture(t, codec, plain)
				if codec == "raw-deflate" {
					encoding = "deflate"
				}
			}
			c := testClient(func(r *http.Request) (*http.Response, error) {
				if !strings.Contains(r.Header.Get("Accept-Encoding"), "br") {
					t.Error("missing supported encodings")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {encoding}}, Body: io.NopCloser(bytes.NewReader(compressed))}, nil
			})
			req, _ := http.NewRequest("GET", "https://local.example", nil)
			data, err := c.doJSON(req, &auth.Auth{})
			if err != nil || string(data) != `{"ok":true}` {
				t.Fatalf("data=%s err=%v", data, err)
			}
		})
	}
	for _, corrupt := range []bool{false, true} {
		compressed := compressFixture(t, "gzip", []byte(strings.Repeat("x", (1<<20)+1)))
		if corrupt {
			compressed = compressed[:len(compressed)-5]
		}
		c := testClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {"gzip"}}, Body: io.NopCloser(bytes.NewReader(compressed))}, nil
		})
		req, _ := http.NewRequest("GET", "https://local.example", nil)
		if _, err := c.doJSON(req, &auth.Auth{}); err == nil {
			t.Fatal("oversized/corrupt decoded response accepted")
		}
	}
}

func TestCompressedSSEIdleTimeout(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		// No compressed bytes arrive: opening the gzip reader must be governed
		// by the same idle timeout as subsequent SSE reads.
		<-r.Context().Done()
	}))
	defer s.Close()
	c := New()
	c.TLSFingerprint = false
	c.ChatBaseCN = s.URL
	c.Stream = StreamPolicy{Idle: 80 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	body, status, _, err := c.ChatStreamContext(ctx, &auth.Auth{UID: "fake", AccessToken: "fake"}, []byte(`{"messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("headers blocked: status=%d err=%v", status, err)
	}
	defer body.Close()
	_, err = io.ReadAll(body)
	if !errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("error=%v", err)
	}
}

func TestCompressedBodyCloseUnblocksInitialization(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := &decodedBody{raw: reader, codec: "gzip"}
	done := make(chan error, 1)
	go func() { var data [1]byte; _, err := body.Read(data[:]); done <- err }()
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock initialization")
	}
}
