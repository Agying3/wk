package upstream

import (
	"bufio"
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/proxy"
)

// Wire capture data, not implementation code. Origin and limitations are
// documented in README.md. Parse a fresh spec for each connection:
// uTLS mutates extension objects while preparing the handshake.
//
//go:embed testdata/workbuddy_clienthello.hex
var workbuddyClientHelloHex string

type fingerprintTransport struct {
	plain *http.Transport
	https *http.Transport
}

func (t *fingerprintTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == "https" {
		return t.https.RoundTrip(r)
	}
	return t.plain.RoundTrip(r)
}

func (t *fingerprintTransport) CloseIdleConnections() {
	t.plain.CloseIdleConnections()
	t.https.CloseIdleConnections()
}

// protocolClient changes only standard transports. Injected RoundTrippers
// (instrumentation, tests) retain their own networking implementation.
func (c *Client) protocolClient(base *http.Client) *http.Client {
	if !c.TLSFingerprint {
		return base
	}
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	tr, ok := rt.(*http.Transport)
	if !ok {
		return base
	}
	actual, ok := c.fingerprintTransports.Load(tr)
	if !ok {
		candidate := newFingerprintTransport(tr)
		actual, _ = c.fingerprintTransports.LoadOrStore(tr, candidate)
	}
	cli := *base
	cli.Transport = actual.(http.RoundTripper)
	return &cli
}

func newFingerprintTransport(base *http.Transport) *fingerprintTransport {
	tr := base.Clone()
	// CONNECT must happen before the custom TLS handshake. Keeping Proxy set
	// would let net/http perform TLS itself inside the proxy tunnel instead.
	tr.Proxy = nil
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Preserve separate connect and TLS budgets. A slow TCP/proxy dial
		// must not spend the destination's TLS handshake timeout.
		tunnelCtx, cancelTunnel := context.WithTimeout(ctx, time.Minute)
		conn, err := fingerprintTunnel(tunnelCtx, base, network, addr)
		cancelTunnel()
		if err != nil {
			return nil, err
		}
		if base.TLSHandshakeTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, base.TLSHandshakeTimeout)
			defer cancel()
		}
		ok := false
		defer func() {
			if !ok {
				conn.Close()
			}
		}()
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, errors.New("invalid TLS destination")
		}
		cfg := &utls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		if b := base.TLSClientConfig; b != nil {
			if len(b.Certificates) != 0 || b.GetClientCertificate != nil || b.VerifyConnection != nil {
				return nil, errors.New("custom client certificates or VerifyConnection require standard TLS")
			}
			cfg.RootCAs, cfg.InsecureSkipVerify = b.RootCAs, b.InsecureSkipVerify
			cfg.VerifyPeerCertificate = b.VerifyPeerCertificate
			if b.ServerName != "" {
				cfg.ServerName = b.ServerName
			}
			if b.MinVersion != 0 {
				cfg.MinVersion = b.MinVersion
			}
			cfg.MaxVersion = b.MaxVersion
		}
		raw, err := hex.DecodeString(strings.TrimSpace(workbuddyClientHelloHex))
		if err != nil {
			return nil, errors.New("invalid bundled TLS capture")
		}
		spec, err := (&utls.Fingerprinter{}).FingerprintClientHello(raw)
		if err != nil {
			return nil, fmt.Errorf("parse bundled TLS profile: %w", err)
		}
		secure := utls.UClient(conn, cfg, utls.HelloCustom)
		if err = secure.ApplyPreset(spec); err != nil {
			return nil, fmt.Errorf("prepare TLS profile: %w", err)
		}
		if err = secure.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("TLS handshake: %w", err)
		}
		if p := secure.ConnectionState().NegotiatedProtocol; p != "" && p != "http/1.1" {
			return nil, errors.New("TLS profile negotiated an unsupported HTTP protocol")
		}
		ok = true
		return secure, nil
	}
	return &fingerprintTransport{plain: base, https: tr}
}

func fingerprintTunnel(ctx context.Context, base *http.Transport, network, addr string) (net.Conn, error) {
	dial := base.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	var px *url.URL
	if base.Proxy != nil {
		var err error
		px, err = base.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
		if err != nil {
			return nil, errors.New("resolve outbound proxy failed")
		}
	}
	if px == nil {
		return dial(ctx, network, addr)
	}
	port := px.Port()
	if port == "" {
		switch px.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			port = "1080"
		}
	}
	proxyAddr := net.JoinHostPort(px.Hostname(), port)
	if px.Scheme == "socks5" || px.Scheme == "socks5h" {
		var credentials *proxy.Auth
		if px.User != nil {
			password, _ := px.User.Password()
			credentials = &proxy.Auth{User: px.User.Username(), Password: password}
		}
		d, err := proxy.SOCKS5("tcp", proxyAddr, credentials, contextProxyDialer{dial})
		if err != nil {
			return nil, errors.New("initialize SOCKS5 proxy failed")
		}
		conn, err := d.(proxy.ContextDialer).DialContext(ctx, network, addr)
		if err != nil {
			return nil, errors.New("SOCKS5 proxy connection failed")
		}
		return conn, nil
	}
	if px.Scheme != "http" && px.Scheme != "https" {
		return nil, errors.New("unsupported outbound proxy scheme")
	}
	conn, err := dial(ctx, network, proxyAddr)
	if err != nil {
		return nil, errors.New("outbound proxy connection failed")
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { rawConn.Close() })
	defer stop()
	if deadline, exists := ctx.Deadline(); exists {
		conn.SetDeadline(deadline)
	}
	if px.Scheme == "https" {
		secure := tls.Client(conn, &tls.Config{ServerName: px.Hostname(), MinVersion: tls.VersionTLS12})
		proxyCtx := ctx
		if base.TLSHandshakeTimeout > 0 {
			var cancel context.CancelFunc
			proxyCtx, cancel = context.WithTimeout(ctx, base.TLSHandshakeTimeout)
			defer cancel()
		}
		if err := secure.HandshakeContext(proxyCtx); err != nil {
			return nil, errors.New("HTTPS proxy TLS handshake failed")
		}
		conn = secure
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: base.ProxyConnectHeader.Clone()}
	if req.Header == nil {
		req.Header = http.Header{}
	}
	if base.GetProxyConnectHeader != nil {
		h, err := base.GetProxyConnectHeader(ctx, px, addr)
		if err != nil {
			return nil, errors.New("prepare proxy CONNECT headers failed")
		}
		req.Header = h.Clone()
		if req.Header == nil {
			req.Header = http.Header{}
		}
	}
	if px.User != nil {
		password, _ := px.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(px.User.Username()+":"+password)))
	}
	if err := req.Write(conn); err != nil {
		return nil, errors.New("write proxy CONNECT failed")
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, errors.New("read proxy CONNECT failed")
	}
	if base.OnProxyConnectResponse != nil {
		if err := base.OnProxyConnectResponse(ctx, px, req, response); err != nil {
			return nil, errors.New("proxy CONNECT response rejected")
		}
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy CONNECT HTTP %d", response.StatusCode)
	}
	if reader.Buffered() != 0 {
		return nil, errors.New("unexpected data after proxy CONNECT")
	}
	conn.SetDeadline(time.Time{})
	ok = true
	return conn, nil
}

type contextProxyDialer struct {
	dial func(context.Context, string, string) (net.Conn, error)
}

func (d contextProxyDialer) Dial(network, addr string) (net.Conn, error) {
	return d.dial(context.Background(), network, addr)
}
func (d contextProxyDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.dial(ctx, network, addr)
}
