package nettransport

// This file holds the round-7 audit regression tests for the CONNECT
// dialer: the default proxy port, the byte budget on the response
// head, and unbounded tunnel reads once that head-only budget is
// lifted.

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

// connectStubHead is the minimal 200 head every stub below answers
// with before any tunnel payload.
const connectStubHead = "HTTP/1.1 200 Connection Established\r\n\r\n"

func TestConnectDialerDefaultsProxyPort(t *testing.T) {
	t.Parallel()

	require.Equal(t, "proxy.example.test:80",
		proxyDialAddr(mustURL("http://proxy.example.test")))
	require.Equal(t, "proxy.example.test:8080",
		proxyDialAddr(mustURL("http://proxy.example.test:8080")))
	require.Equal(t, "[2001:db8::1]:80",
		proxyDialAddr(mustURL("http://[2001:db8::1]")))
	require.Equal(t, "[2001:db8::1]:8443",
		proxyDialAddr(mustURL("http://[2001:db8::1]:8443")))
}

// drainConnectRequest reads and discards the client's CONNECT request
// head (through the terminating blank line) before a stub answers.
// Skipping this leaves the request bytes unread in the socket's receive
// buffer when the stub later closes the connection: on Windows that
// turns what looks like a graceful close into an RST, which can arrive
// mid-read on the client side as "connection forcibly closed by the
// remote host". This surfaced as an apparent flake in two sibling
// stubs below that wrote their response without ever reading the
// request first -- reproducible under concurrent package load (-p >1),
// where the client is more likely to still be mid-read when the stub's
// goroutine returns and its deferred Close fires, and effectively
// unreproducible running the package alone, where the client almost
// always finishes reading first. Draining the request removes the
// precondition for the RST entirely, regardless of timing.
func drainConnectRequest(conn net.Conn) error {
	head := make([]byte, 0, 4096)
	buf := make([]byte, 1024)
	for !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
		n, err := conn.Read(buf)
		head = append(head, buf[:n]...)
		if err != nil {
			return err
		}
	}
	return nil
}

// dialThroughStub drives connectDialer against the stub listener with
// the always-unresolvable authority "tunnel-target.invalid:443" and
// returns the raw dial outcome; the test aborts when the dial does
// not settle within testAbortWait.
func dialThroughStub(t *testing.T, ln net.Listener) (net.Conn, error) {
	t.Helper()

	type dialResult struct {
		conn net.Conn
		err  error
	}
	ctx := t.Context()
	ch := make(chan dialResult, 1)
	go func() {
		conn, err := connectDialer(mustURL("http://"+ln.Addr().String()))(
			ctx, "tcp", "tunnel-target.invalid:443")
		ch <- dialResult{conn: conn, err: err}
	}()
	select {
	case r := <-ch:
		return r.conn, r.err
	case <-time.After(testAbortWait):
		t.Fatal("connectDialer did not return within testAbortWait")
		return nil, nil
	}
}

func TestConnectResponseHeadByteBounded(t *testing.T) {
	t.Parallel()

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	// stubDone closes once the stub has flooded past the head budget
	// and then observed a write failure or the client's close.
	stubDone := make(chan struct{})
	go func() {
		defer close(stubDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// Consume the CONNECT request head before answering (see
		// drainConnectRequest's doc for why this must happen before Close).
		if err := drainConnectRequest(conn); err != nil {
			return
		}

		// A real status line first: without one, http.ReadResponse
		// rejects the very first ~1000-byte line as "malformed HTTP
		// status code" long before the byte budget is ever tested,
		// which is a different failure than the one this test pins.
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n")); err != nil {
			return
		}
		// Flood header lines far past the client's head budget; a
		// failing write is the client's close surfacing.
		pad := "X-Pad: " + strings.Repeat("A", 1000) + "\r\n"
		for written := 0; written < 4*int(maxConnectResponseHead); {
			n, err := conn.Write([]byte(pad))
			written += n
			if err != nil {
				return
			}
		}

		// The flood may still fit in kernel buffers; keep poking the
		// socket until the close surfaces or the abort wait expires.
		deadline := time.Now().Add(testAbortWait)
		for time.Now().Before(deadline) {
			if _, err := conn.Write([]byte(pad)); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	conn, err := dialThroughStub(t, ln)
	require.Error(t, err, "an over-budget CONNECT head must fail the dial")
	require.Nil(t, conn)
	require.Contains(t, err.Error(), "head limit")

	select {
	case <-stubDone:
	case <-time.After(testAbortWait):
		t.Fatal("stub never observed a write failure or client close")
	}
}

func TestConnectTunnelKeepsBufferedPastHeadBytes(t *testing.T) {
	t.Parallel()

	payload := []byte("tunnel-payload-bytes")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if err := drainConnectRequest(conn); err != nil {
			return
		}
		// Head and payload leave in a single write, before the
		// client has parsed anything.
		_, _ = conn.Write(append([]byte(connectStubHead), payload...))
	}()

	conn, err := dialThroughStub(t, ln)
	require.NoError(t, err)
	require.NotNil(t, conn)
	defer func() { _ = conn.Close() }()

	got := make([]byte, len(payload))
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestConnectTunnelReadsBeyondHeadBudget(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte{0xA5}, int(maxConnectResponseHead)+4096)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if err := drainConnectRequest(conn); err != nil {
			return
		}
		if _, err := conn.Write([]byte(connectStubHead)); err != nil {
			return
		}
		// Stream the tunnel payload in chunks while the client
		// reads it far past the response-head budget.
		const chunk = 4096
		for off := 0; off < len(payload); off += chunk {
			end := min(off+chunk, len(payload))
			if _, err := conn.Write(payload[off:end]); err != nil {
				return
			}
		}
	}()

	conn, err := dialThroughStub(t, ln)
	require.NoError(t, err)
	require.NotNil(t, conn)
	defer func() { _ = conn.Close() }()

	got := make([]byte, len(payload))
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

// TestDoHTransportBoundedDialContext pins the R6-1 fix: every DoH
// transport carries a non-nil DialContext on all three proxy routes —
// no proxy, HTTP proxy, and SOCKS5 — so stdlib's zero dialer never
// governs the initial TCP connect to the endpoint.
func TestDoHTransportBoundedDialContext(t *testing.T) {
	t.Parallel()

	const endpoint = "https://doh.example.invalid/dns-query"

	_, trDirect, err := newDoHResolver(endpoint, nil, time.Second)
	require.NoError(t, err)
	require.Nil(t, trDirect.Proxy)
	require.NotNil(t, trDirect.DialContext)
	t.Cleanup(trDirect.CloseIdleConnections)

	_, trHTTP, err := newDoHResolver(endpoint,
		mustURL("http://127.0.0.1:1"), time.Second)
	require.NoError(t, err)
	require.NotNil(t, trHTTP.Proxy)
	require.NotNil(t, trHTTP.DialContext)
	t.Cleanup(trHTTP.CloseIdleConnections)

	_, trSocks, err := newDoHResolver(endpoint,
		mustURL("socks5://127.0.0.1:1"), time.Second)
	require.NoError(t, err)
	require.NotNil(t, trSocks.DialContext) // Set by the SOCKS dialer.
	t.Cleanup(trSocks.CloseIdleConnections)
}

// TestResolvedDialerFallsBackAcrossAddresses proves the R4-2 fix: a
// resolver returning several addresses makes resolvedDialer attempt
// them in order and succeed on a later one instead of failing after
// the first.
func TestResolvedDialerFallsBackAcrossAddresses(t *testing.T) {
	t.Parallel()

	_, targetHostport := startTargetServer(t)
	targetPort := requirePort(t, targetHostport)
	resolver := func(_ context.Context, _ string) ([]net.IP, error) {
		return []net.IP{
			net.IPv4(127, 0, 0, 2).To4(),
			net.IPv4(127, 0, 0, 1).To4(),
		}, nil
	}
	dial := resolvedDialer(resolver, nil)

	conn, err := dial(t.Context(), "tcp", "fallback.invalid:"+strconv.Itoa(targetPort))
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:"+strconv.Itoa(targetPort), conn.RemoteAddr().String())
	require.NoError(t, conn.Close())
}

// TestResolvedDialerReportsAllFailures proves the R4-2 error surface:
// when every resolved address fails, the returned error names each
// attempted address via errors.Join, not just the last one.
func TestResolvedDialerReportsAllFailures(t *testing.T) {
	t.Parallel()

	resolver := func(_ context.Context, _ string) ([]net.IP, error) {
		return []net.IP{
			net.IPv4(127, 0, 0, 2).To4(),
			net.IPv4(127, 0, 0, 3).To4(),
		}, nil
	}
	dial := resolvedDialer(resolver, nil)

	_, err := dial(t.Context(), "tcp", "unreachable.invalid:1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "127.0.0.2")
	require.Contains(t, err.Error(), "127.0.0.3")
}

// TestDNSOverTCPResolverQueriesBothFamiliesUnconditionally proves the R4-2
// residual fix (2026-09-22 round-8 audit): the resolver must query AAAA
// even when the A query already succeeded, because DNS resolving an
// address proves nothing about whether the dialer can actually reach it —
// an IPv4-only-blocked route can leave every A address undialable while an
// AAAA address for the same name works fine. startDNSServer's double
// answers BOTH question types from the same answerIP (its 4-byte and
// 16-byte forms respectively), so a combined result of length 2 proves
// both queries actually happened and were merged, not just the first one.
func TestDNSOverTCPResolverQueriesBothFamiliesUnconditionally(t *testing.T) {
	t.Parallel()

	dnsSrv := startDNSServer(t, net.ParseIP("127.0.0.1"))
	resolver := dnsOverTCPResolver(dnsSrv.tcpAddr(), directDialFunc())

	ips, err := resolver(t.Context(), "both-families.invalid")
	require.NoError(t, err)
	require.Len(t, ips, 2, "must combine the A and the AAAA answer, not stop at A")
	// answerIPs packs an A record's 4 raw bytes and an AAAA record's 16
	// raw bytes as-is (net.IP(a.A[:]) / net.IP(aaaa.AAAA[:])), so the
	// slice LENGTH tells the two apart even though both encode the same
	// loopback address here -- To4() would not: it un-maps a 16-byte
	// IPv4-mapped IPv6 address back to 4 bytes, so it is non-nil for
	// EITHER answer and cannot distinguish them.
	require.Len(t, ips[0], net.IPv4len, "the A answer must come first")
	require.Len(t, ips[1], net.IPv6len, "the AAAA answer must be present as the second address")
	require.Equal(t, 2, dnsSrv.hitsTCP(), "both the A and the AAAA query must have reached the server")
}

// TestNewDoHResolverQueriesBothFamiliesUnconditionally is the DoH-transport
// twin of TestDNSOverTCPResolverQueriesBothFamiliesUnconditionally.
func TestNewDoHResolverQueriesBothFamiliesUnconditionally(t *testing.T) {
	t.Parallel()

	doh := startDoHServer(t, net.ParseIP("127.0.0.1"))
	resolve, tr, err := newDoHResolver(doh.URL(), nil, time.Second)
	require.NoError(t, err)
	t.Cleanup(tr.CloseIdleConnections)

	ips, err := resolve(t.Context(), "both-families.invalid")
	require.NoError(t, err)
	require.Len(t, ips, 2, "must combine the A and the AAAA answer, not stop at A")
	// See the sibling DNS-over-TCP test above for why length, not To4(),
	// distinguishes the two answers.
	require.Len(t, ips[0], net.IPv4len, "the A answer must come first")
	require.Len(t, ips[1], net.IPv6len, "the AAAA answer must be present as the second address")
	require.Equal(t, 2, doh.hits(), "both the A and the AAAA query must have reached the endpoint")
}

// TestAnswerIPsCollectsAllAnswers proves answerIPs keeps every A and
// AAAA answer record in message order and reports an empty answer
// section as (nil, false).
func TestAnswerIPsCollectsAllAnswers(t *testing.T) {
	t.Parallel()

	msg := &dnsmessage.Message{
		Answers: []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{Type: dnsmessage.TypeA},
				Body:   &dnsmessage.AResource{A: [4]byte{1, 2, 3, 4}},
			},
			{
				Header: dnsmessage.ResourceHeader{Type: dnsmessage.TypeA},
				Body:   &dnsmessage.AResource{A: [4]byte{5, 6, 7, 8}},
			},
			{
				Header: dnsmessage.ResourceHeader{Type: dnsmessage.TypeAAAA},
				Body:   &dnsmessage.AAAAResource{AAAA: [16]byte{14: 0xAA, 15: 0xAA}},
			},
		},
	}
	want := []net.IP{
		net.IPv4(1, 2, 3, 4).To4(),
		net.IPv4(5, 6, 7, 8).To4(),
		net.ParseIP("::aaaa"),
	}
	ips, ok := answerIPs(msg)
	require.True(t, ok)
	require.Equal(t, want, ips)

	ips, ok = answerIPs(&dnsmessage.Message{})
	require.False(t, ok)
	require.Nil(t, ips)
}

// TestRedactedURLStripsSensitiveComponents pins the R2-5 redaction
// helpers: userinfo, query and fragment disappear from rendered URLs,
// IPv6 and path survive, and input that does not parse degrades to a
// fixed placeholder instead of echoing secret-bearing text.
func TestRedactedURLStripsSensitiveComponents(t *testing.T) {
	t.Parallel()

	require.Equal(t, "socks5://127.0.0.1:1080",
		redactedURL("socks5://user:pass@127.0.0.1:1080"))
	require.Equal(t, "https://doh.example.invalid/dns-query",
		redactedURL("https://key@doh.example.invalid/dns-query?q=1#frag"))
	require.Equal(t, "http://[::1]:8080/x", redactedURL("http://[::1]:8080/x"))
	// The control byte makes url.Parse fail, so the raw input must
	// never be echoed back.
	require.Equal(t, "(unparseable URL)",
		redactedURL("http://user:secret@host.invalid/\x00"))
	require.Equal(t, "http://h.example.invalid:1/a",
		redactedURLValue(mustURL("http://u:p@h.example.invalid:1/a?b=c")))
}

// TestResolveConfigErrorRedactsSecrets proves the R2-5 fix on both
// resolveConfig failure paths: proxy secrets never reach the error
// text, and a malformed DoH URL is reported via the placeholder
// instead of the nested *url.Error echo of the raw input.
func TestResolveConfigErrorRedactsSecrets(t *testing.T) {
	t.Parallel()

	_, err := resolveConfig(config.NetworkConfig{
		Proxy: "ftp://user:secret@proxy.example.test:21/?token=shh#frag",
	})
	require.Error(t, err)
	msg := err.Error()
	require.NotContains(t, msg, "secret")
	require.NotContains(t, msg, "shh")
	require.NotContains(t, msg, "frag")
	require.NotContains(t, msg, "user:")
	require.Contains(t, msg, "ftp://proxy.example.test:21")

	_, err = resolveConfig(config.NetworkConfig{DoHURL: "https://[::1:443"})
	require.Error(t, err)
	msg = err.Error()
	require.NotContains(t, msg, "https://[::1:443")
	require.Contains(t, msg, "(unparseable URL)")
}

// TestDoHErrorsRedactEndpoint proves the R2-5 fix behaviorally on the
// DoH error path: the failing endpoint's query-string secret stays out
// of the error while scheme, host and path remain for diagnosis.
func TestDoHErrorsRedactEndpoint(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
	t.Cleanup(server.Close)
	endpoint := server.URL + "/dns-query?apikey=shh"

	resolve, tr, err := newDoHResolver(endpoint, nil, time.Second)
	require.NoError(t, err)
	t.Cleanup(tr.CloseIdleConnections)

	_, err = resolve(t.Context(), "x.invalid")
	require.Error(t, err)
	msg := err.Error()
	require.NotContains(t, msg, "apikey=shh")
	require.Contains(t, msg, "/dns-query")
}

// TestDoHConnectionFailureRedactsEndpoint proves the R2-5 residual fix
// (2026-09-22 round-8 audit): a DoH endpoint with a secret in its query
// that fails at the CONNECTION level -- not the clean HTTP 503 the sibling
// test above covers -- must still keep that secret out of the returned
// error. net/http wraps a dial failure in a *url.Error whose own Error()
// string embeds the raw URL including the query; that string would have
// survived the outer %w wrap even though the surrounding message text
// already used the safe redacted endpoint.
func TestDoHConnectionFailureRedactsEndpoint(t *testing.T) {
	t.Parallel()

	// A loopback port nothing listens on: bind then close immediately, so
	// the connection is refused promptly instead of hanging on a dead
	// address.
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	endpoint := "http://" + addr + "/dns-query?apikey=shh"
	resolve, tr, err := newDoHResolver(endpoint, nil, time.Second)
	require.NoError(t, err)
	t.Cleanup(tr.CloseIdleConnections)

	_, err = resolve(t.Context(), "x.invalid")
	require.Error(t, err)
	msg := err.Error()
	require.NotContains(t, msg, "shh")
	require.NotContains(t, msg, "apikey")
	require.Contains(t, msg, "http://"+addr+"/dns-query")
}
