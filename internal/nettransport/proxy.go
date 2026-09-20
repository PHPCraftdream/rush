package nettransport

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// dialFunc dials addr ("host:port"), routing through a proxy when one
// is configured. addr must often stay a HOSTNAME rather than a
// pre-resolved IP: in proxy-only mode the proxy is responsible for
// name resolution (a SOCKS5 server receives the raw hostname per RFC
// 1928 ATYP=0x03), so pre-resolving here would change who resolves.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// proxyDialer returns a dialFunc that routes every dial through the
// given proxy URL, dispatched by scheme.
func proxyDialer(pu *url.URL) (dialFunc, error) {
	switch pu.Scheme {
	case "socks5", "socks5h":
		return socksDialer(pu)
	case "http":
		return connectDialer(pu), nil
	default:
		return nil, fmt.Errorf(
			"unsupported proxy scheme %q in %q (supported: socks5, socks5h, http)",
			pu.Scheme, pu.String())
	}
}

// socksDialer builds a SOCKS5 dialer.
//
// CRITICAL: DialContext is invoked with the ORIGINAL "host:port" —
// when the host is not an IP literal, x/net's socks layer sends the
// domain name to the server (ATYP=0x03) so the SOCKS5 server resolves
// it. Do not pre-resolve the host here.
func socksDialer(pu *url.URL) (dialFunc, error) {
	var auth *proxy.Auth
	if u := pu.User; u != nil {
		// Any username enables auth; a missing password is "".
		pass, _ := u.Password()
		auth = &proxy.Auth{User: u.Username(), Password: pass}
	}
	d, err := proxy.SOCKS5("tcp", pu.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("create SOCKS5 dialer for proxy %q: %w", pu.Host, err)
	}
	// The SOCKS5 dialer implements proxy.ContextDialer on x/net
	// v0.55.0; assert so the context is honored instead of dropped.
	ctxDialer, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf(
			"SOCKS5 dialer for proxy %q does not implement proxy.ContextDialer", pu.Host)
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return ctxDialer.DialContext(ctx, network, addr)
	}, nil
}

// connectDialer returns a dialFunc that tunnels through an HTTP proxy
// using the CONNECT method. Each dial opens a fresh TCP connection to
// the proxy, sends "CONNECT <addr>", and on a 200 response returns the
// raw connection as the tunnel.
func connectDialer(pu *url.URL) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		// The proxy tunnel itself is always plain TCP, regardless of
		// what the target connection will carry inside it.
		d := net.Dialer{Timeout: 30 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", pu.Host)
		if err != nil {
			return nil, fmt.Errorf("dial proxy %q: %w", pu.Host, err)
		}
		if err := writeConnectRequest(conn, pu, addr); err != nil {
			_ = conn.Close()
			return nil, err
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("read CONNECT response from proxy %q: %w", pu.Host, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = conn.Close()
			_ = resp.Body.Close()
			return nil, fmt.Errorf(
				"proxy %q refused CONNECT to %q: %s", pu.Host, addr, resp.Status)
		}
		// The bufio.Reader may have buffered bytes beyond the response
		// head; bufferedConn drains them first so a TLS handshake over
		// the tunnel never loses bytes.
		return &bufferedConn{Conn: conn, r: br}, nil
	}
}

// writeConnectRequest sends the HTTP CONNECT request for addr over the
// raw proxy connection. The Basic token is built from the proxy URL
// userinfo; the joined "user:pass" string is base64-encoded directly
// because url.Userinfo.String() URL-escapes, which would corrupt the
// header value.
func writeConnectRequest(conn net.Conn, pu *url.URL, addr string) error {
	var token string
	if u := pu.User; u != nil {
		pass, _ := u.Password()
		token = base64.StdEncoding.EncodeToString([]byte(u.Username() + ":" + pass))
	}
	var b strings.Builder
	b.WriteString("CONNECT " + addr + " HTTP/1.1\r\n")
	b.WriteString("Host: " + addr + "\r\n")
	if token != "" {
		b.WriteString("Proxy-Authorization: Basic " + token + "\r\n")
	}
	b.WriteString("\r\n")
	if _, err := conn.Write([]byte(b.String())); err != nil {
		return fmt.Errorf("send CONNECT request to proxy %q: %w", pu.Host, err)
	}
	return nil
}

// bufferedConn is a net.Conn whose response head was parsed through a
// bufio.Reader; Read drains whatever the reader already buffered
// before falling back to the underlying connection, so the tunnel
// stream stays byte-exact.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

// Read first serves bytes already buffered by the response-head parser.
func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}
