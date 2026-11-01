package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"workbuddy2api/internal/auth"
)

func tlsFixture(t *testing.T) (*httptest.Server, *x509.CertPool, <-chan *tls.ClientHelloInfo) {
	t.Helper()
	hellos := make(chan *tls.ClientHelloInfo, 8)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(30 * time.Millisecond)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	s.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		hellos <- hello
		return nil, nil
	}}
	s.StartTLS()
	t.Cleanup(s.Close)
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	return s, roots, hellos
}

func connectFixture(t *testing.T) (*url.URL, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("proxy received %s instead of CONNECT", r.Method)
			w.WriteHeader(400)
			return
		}
		hits.Add(1)
		target, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			target.Close()
			return
		}
		defer conn.Close()
		defer target.Close()
		fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { io.Copy(target, conn); target.Close(); close(done) }()
		io.Copy(conn, target)
		conn.Close()
		<-done
	}))
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	return u, hits
}

func TestFingerprintTransportDirectAndCONNECT(t *testing.T) {
	for _, proxied := range []bool{false, true} {
		t.Run(fmt.Sprint(proxied), func(t *testing.T) {
			s, roots, hellos := tlsFixture(t)
			base := http.DefaultTransport.(*http.Transport).Clone()
			base.Proxy = nil
			base.TLSClientConfig = &tls.Config{RootCAs: roots}
			var hits *atomic.Int32
			if proxied {
				px, count := connectFixture(t)
				hits = count
				base.Proxy = http.ProxyURL(px)
			}
			transport := newFingerprintTransport(base)
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			for i := 0; i < 2; i++ {
				resp, err := client.Get(s.URL)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || !strings.Contains(string(body), "[DONE]") || resp.ProtoMajor != 1 {
					t.Fatalf("incomplete HTTP/1.1 stream: %s, %v", body, err)
				}
			}
			hello := <-hellos
			raw, _ := hex.DecodeString(strings.TrimSpace(workbuddyClientHelloHex))
			spec, err := (&utls.Fingerprinter{}).FingerprintClientHello(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(hello.CipherSuites, spec.CipherSuites) || !reflect.DeepEqual(hello.SupportedProtos, []string{"http/1.1"}) {
				t.Fatal("actual handshake differs from the selected profile")
			}
			if hello.ServerName != "" {
				t.Fatal("captured hostname leaked into a loopback-IP handshake")
			}
			if len(hellos) != 0 || hits != nil && hits.Load() != 1 {
				t.Fatal("connection was not reused")
			}
		})
	}
}

func TestFingerprintTransportRejectsUntrustedCertificate(t *testing.T) {
	s, _, _ := tlsFixture(t)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	tr := newFingerprintTransport(base)
	defer tr.CloseIdleConnections()
	_, err := (&http.Client{Transport: tr, Timeout: time.Second}).Get(s.URL)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted server was not rejected: %v", err)
	}
}

func TestFingerprintTransportUsesDestinationSNI(t *testing.T) {
	s, roots, hellos := tlsFixture(t)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.TLSClientConfig = &tls.Config{RootCAs: roots}
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, s.Listener.Addr().String())
	}
	tr := newFingerprintTransport(base)
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: time.Second}).Get("https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if hello := <-hellos; hello.ServerName != "example.com" {
		t.Fatal("capture SNI replaced the real destination")
	}
}

func TestFingerprintTransportTLSBudgetStartsAfterDial(t *testing.T) {
	s, roots, _ := tlsFixture(t)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.TLSClientConfig = &tls.Config{RootCAs: roots}
	base.TLSHandshakeTimeout = 60 * time.Millisecond
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
	}
	tr := newFingerprintTransport(base)
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: time.Second}).Get(s.URL)
	if err != nil {
		t.Fatal("TCP dial incorrectly consumed TLS budget:", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestFingerprintTransportProxyTimeoutAndNoDirectFallback(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	release := make(chan struct{})
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer px.Close()
	defer close(release)
	proxyURL, _ := url.Parse(px.URL)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = http.ProxyURL(proxyURL)
	base.TLSHandshakeTimeout = 80 * time.Millisecond
	tr := newFingerprintTransport(base)
	defer tr.CloseIdleConnections()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	_, err := (&http.Client{Transport: tr, Timeout: time.Second}).Do(req)
	if err == nil || time.Since(start) > 800*time.Millisecond || targetHits.Load() != 0 {
		t.Fatalf("proxy timeout/fallback: err=%v target_hits=%d", err, targetHits.Load())
	}
}

func TestFingerprintTransportAccountProxySelectionAndConcurrentCache(t *testing.T) {
	first, hits := connectFixture(t)
	c := New()
	c.TLSFingerprint = true
	c.DialProxy = func(uid, _ string) (*url.URL, error) {
		if uid != "fixture-account" {
			return nil, errNoNode
		}
		return first, nil
	}
	var wg sync.WaitGroup
	clients := make(chan *http.Client, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cli, err := c.proxyClientForContext(context.Background(), &auth.Auth{UID: "fixture-account"})
			if err != nil {
				t.Error(err)
				return
			}
			clients <- cli
		}()
	}
	wg.Wait()
	close(clients)
	var shared http.RoundTripper
	for cli := range clients {
		if shared != nil && shared != cli.Transport {
			t.Fatal("cache created multiple active connection pools")
		}
		shared = cli.Transport
	}
	defer shared.(*fingerprintTransport).CloseIdleConnections()
	if _, err := c.proxyClientForContext(context.Background(), &auth.Auth{UID: "unassigned"}); err == nil || hits.Load() != 0 {
		t.Fatal("proxy selection was bypassed")
	}
}

func TestFingerprintTransportSOCKS5AuthenticatedTunnel(t *testing.T) {
	s, roots, _ := tlsFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	proxyDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			proxyDone <- err
			return
		}
		defer conn.Close()
		err = serveSOCKSFixture(conn)
		proxyDone <- err
	}()
	px := &url.URL{Scheme: "socks5", Host: listener.Addr().String(), User: url.UserPassword("fixture", "dummy-password")}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = http.ProxyURL(px)
	base.TLSClientConfig = &tls.Config{RootCAs: roots}
	tr := newFingerprintTransport(base)
	resp, err := (&http.Client{Transport: tr, Timeout: time.Second}).Get(s.URL)
	if err != nil {
		tr.CloseIdleConnections()
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	tr.CloseIdleConnections()
	if err != nil || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("SOCKS5 stream: %s, %v", body, err)
	}
	select {
	case err := <-proxyDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("SOCKS5 tunnel did not close")
	}
}

func serveSOCKSFixture(conn net.Conn) error {
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	conn.Write([]byte{5, 2}) // RFC 1929 username/password authentication.
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	username := make([]byte, int(header[1]))
	io.ReadFull(conn, username)
	length := make([]byte, 1)
	io.ReadFull(conn, length)
	password := make([]byte, int(length[0]))
	io.ReadFull(conn, password)
	if string(username) != "fixture" || string(password) != "dummy-password" {
		return fmt.Errorf("fixture proxy authentication mismatch")
	}
	conn.Write([]byte{1, 0})
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return err
	}
	var host string
	switch request[3] {
	case 1:
		ip := make([]byte, 4)
		io.ReadFull(conn, ip)
		host = net.IP(ip).String()
	case 3:
		io.ReadFull(conn, length)
		domain := make([]byte, int(length[0]))
		io.ReadFull(conn, domain)
		host = string(domain)
	default:
		return fmt.Errorf("unexpected fixture address type")
	}
	port := make([]byte, 2)
	io.ReadFull(conn, port)
	target, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(port))), time.Second)
	if err != nil {
		return err
	}
	defer target.Close()
	conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
	copyDone := make(chan struct{})
	go func() { io.Copy(target, conn); target.Close(); close(copyDone) }()
	io.Copy(conn, target)
	conn.Close()
	<-copyDone
	return nil
}
