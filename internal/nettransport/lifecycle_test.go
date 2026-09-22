package nettransport

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// testAbortWait bounds every wait on a cancelled or timed-out
// handshake in these tests. It is deliberately well under the 30
// second handshake budgets, so a pass proves the dial returned because
// of cancellation — not because the budget ran out — and a bug can
// only fail the test, never hang it.
const testAbortWait = 3 * time.Second

// directDialFunc is a plain TCP dialFunc, for testing resolvers
// without a proxy in front.
func directDialFunc() dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
}

// silentConnectStub accepts TCP connections, reads one CONNECT request
// head, signals its arrival, and then never responds — the F4 premise:
// the proxy accepted the tunnel connection but never answers CONNECT.
type silentConnectStub struct {
	ln      net.Listener
	gotOnce chan struct{}
	once    sync.Once
	live    atomic.Int64
}

func startSilentConnectStub(t *testing.T) *silentConnectStub {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &silentConnectStub{ln: ln, gotOnce: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

func (s *silentConnectStub) addr() string { return s.ln.Addr().String() }

// liveCount reports how many accepted stub connections have not ended
// yet; a connection ends only when the client side closes it.
func (s *silentConnectStub) liveCount() int64 { return s.live.Load() }

// waitGotRequest blocks until the stub observed one CONNECT head,
// bounded so a client that never even connected fails fast.
func (s *silentConnectStub) waitGotRequest(t *testing.T) {
	t.Helper()
	select {
	case <-s.gotOnce:
	case <-time.After(testAbortWait):
		t.Fatal("stub never observed the CONNECT request head")
	}
}

func (s *silentConnectStub) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.live.Add(1)
		go s.handleConn(conn)
	}
}

// handleConn reads the CONNECT head, signals, then blocks on reads
// until the connection ends, so liveCount observes client-side closes.
func (s *silentConnectStub) handleConn(conn net.Conn) {
	defer s.live.Add(-1)
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	head := make([]byte, 0, 512)
	buf := make([]byte, 256)
	for !bytes.Contains(head, []byte("\r\n\r\n")) {
		if len(head) > 8<<10 {
			return
		}
		n, err := conn.Read(buf)
		if n > 0 {
			head = append(head, buf[:n]...)
		}
		if err != nil {
			return
		}
	}
	s.once.Do(func() { close(s.gotOnce) })

	var sink [1]byte
	for {
		if _, err := conn.Read(sink[:]); err != nil {
			return
		}
	}
}

// silentDNSStub accepts a framed DNS-over-TCP query, signals its
// arrival, and never responds.
type silentDNSStub struct {
	ln      net.Listener
	gotOnce chan struct{}
	once    sync.Once
	live    atomic.Int64
}

func startSilentDNSStub(t *testing.T) *silentDNSStub {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &silentDNSStub{ln: ln, gotOnce: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

func (s *silentDNSStub) addr() string { return s.ln.Addr().String() }

func (s *silentDNSStub) liveCount() int64 { return s.live.Load() }

func (s *silentDNSStub) waitGotQuery(t *testing.T) {
	t.Helper()
	select {
	case <-s.gotOnce:
	case <-time.After(testAbortWait):
		t.Fatal("stub never observed the DNS query")
	}
}

func (s *silentDNSStub) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.live.Add(1)
		go s.handleConn(conn)
	}
}

func (s *silentDNSStub) handleConn(conn net.Conn) {
	defer s.live.Add(-1)
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	var prefix [2]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		return
	}
	length := int(binary.BigEndian.Uint16(prefix[:]))
	if length > 0 {
		msg := make([]byte, length)
		if _, err := io.ReadFull(conn, msg); err != nil {
			return
		}
	}
	s.once.Do(func() { close(s.gotOnce) })

	var sink [1]byte
	for {
		if _, err := conn.Read(sink[:]); err != nil {
			return
		}
	}
}

// silentDoHStub is an RFC 8484 endpoint that accepts the POST, signals
// its arrival, and never responds.
type silentDoHStub struct {
	ts      *httptest.Server
	gotOnce chan struct{}
	done    chan struct{}
	once    sync.Once
}

func startSilentDoHStub(t *testing.T) *silentDoHStub {
	t.Helper()
	s := &silentDoHStub{gotOnce: make(chan struct{}), done: make(chan struct{})}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.once.Do(func() { close(s.gotOnce) })
		select {
		case <-r.Context().Done():
		case <-s.done:
		}
	}))
	t.Cleanup(func() {
		close(s.done)
		s.ts.Close()
	})
	return s
}

func (s *silentDoHStub) url() string { return s.ts.URL }

func (s *silentDoHStub) waitGotRequest(t *testing.T) {
	t.Helper()
	select {
	case <-s.gotOnce:
	case <-time.After(testAbortWait):
		t.Fatal("stub never observed the DoH request")
	}
}

// TestConnectHandshakeRespectsContextCancellation proves the F4 fix:
// a CONNECT handshake in flight against a silent proxy returns an
// error promptly when the caller's context is cancelled, and closes
// the underlying socket instead of leaving it dangling.
func TestConnectHandshakeRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	stub := startSilentConnectStub(t)
	dial := connectDialer(mustURL("http://" + stub.addr()))

	ctx, cancel := context.WithCancel(context.Background())
	type dialResult struct {
		conn net.Conn
		err  error
	}
	result := make(chan dialResult, 1)
	go func() {
		conn, err := dial(ctx, "tcp", "tunnel-target.invalid:443")
		result <- dialResult{conn: conn, err: err}
	}()

	// Establish the premise: the handshake is genuinely in flight —
	// the proxy accepted AND received the CONNECT request — before we
	// cancel. Not "the dial itself was slow".
	stub.waitGotRequest(t)
	cancel()

	select {
	case res := <-result:
		require.Error(t, res.err, "cancelled CONNECT handshake must fail")
		require.Nil(t, res.conn)
	case <-time.After(testAbortWait):
		t.Fatal("cancelled CONNECT handshake did not return within the bound")
	}

	// The cancelled dial must have closed the underlying socket, not
	// just returned: the stub's connection ends only when the client
	// side is really gone.
	require.Eventually(t, func() bool { return stub.liveCount() == 0 },
		5*time.Second, 50*time.Millisecond,
		"stub connection must be closed after cancellation")
}

// TestDNSTCPQueryRespectsContextCancellation proves the F4 fix for
// DNS-over-TCP: a query in flight against a silent server fails
// promptly on cancellation and closes the socket.
func TestDNSTCPQueryRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	stub := startSilentDNSStub(t)
	resolver := dnsOverTCPResolver(stub.addr(), directDialFunc())

	ctx, cancel := context.WithCancel(context.Background())
	type queryResult struct {
		ips []net.IP
		err error
	}
	result := make(chan queryResult, 1)
	go func() {
		ips, err := resolver(ctx, "silent.invalid")
		result <- queryResult{ips: ips, err: err}
	}()

	stub.waitGotQuery(t)
	cancel()

	select {
	case res := <-result:
		require.Error(t, res.err, "cancelled DNS-over-TCP query must fail")
		require.Nil(t, res.ips)
	case <-time.After(testAbortWait):
		t.Fatal("cancelled DNS query did not return within the bound")
	}

	require.Eventually(t, func() bool { return stub.liveCount() == 0 },
		5*time.Second, 50*time.Millisecond,
		"stub connection must be closed after cancellation")
}

// TestDoHLookupClientOwnedTimeout proves the F4 fix's DoH half: the
// lookup is bounded by the DoH client's OWN timeout even though the
// caller's context is never cancelled — necessary because net/http
// detaches the dial context from the request's cancellation
// (getConn's context.WithoutCancel). This is the test that fails
// without the fix: the lookup would only return when the 5s outer
// context fires, blowing the 3s bound.
func TestDoHLookupClientOwnedTimeout(t *testing.T) {
	t.Parallel()

	stub := startSilentDoHStub(t)
	resolve, tr, err := newDoHResolver(stub.url(), nil, 250*time.Millisecond)
	require.NoError(t, err)
	t.Cleanup(tr.CloseIdleConnections)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	ips, err := resolve(ctx, "silent.invalid")
	elapsed := time.Since(start)

	require.Error(t, err, "DoH lookup against a silent endpoint must fail on the client-owned timeout")
	require.Nil(t, ips)
	require.Less(t, elapsed, testAbortWait,
		"the client-owned timeout must bound the lookup well before the outer context")
}

// TestDoHLookupRespectsContextCancellation is the cancel-equivalent of
// the required DoH scenario: cancel after the endpoint received the
// request. (net/http aborts the in-flight round trip on its own once
// the request is in flight; the client-owned timeout above is what
// covers the detached dial phase.)
func TestDoHLookupRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	stub := startSilentDoHStub(t)
	resolve, tr, err := newDoHResolver(stub.url(), nil, dohClientTimeout)
	require.NoError(t, err)
	t.Cleanup(tr.CloseIdleConnections)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := resolve(ctx, "silent.invalid")
		result <- err
	}()

	stub.waitGotRequest(t)
	cancel()

	select {
	case err := <-result:
		require.Error(t, err, "cancelled DoH lookup must fail")
	case <-time.After(testAbortWait):
		t.Fatal("cancelled DoH lookup did not return within the bound")
	}
}

// setSocksHandshakeTimeoutForTest shortens the SOCKS5 handshake budget
// for the duration of one test and restores it afterwards. The budget
// is a package var because these tests drive BuildHTTPClient, which
// offers no parameter channel for it; tests calling this helper are
// deliberately serial (no t.Parallel) so no parallel test can observe
// the shortened budget.
func setSocksHandshakeTimeoutForTest(t *testing.T, d time.Duration) {
	t.Helper()
	old := socksHandshakeTimeout
	socksHandshakeTimeout = d
	t.Cleanup(func() { socksHandshakeTimeout = old })
}

// silentSocksStub accepts TCP connections, reads one RFC 1928
// greeting, answers a valid no-auth method selection so the client
// proceeds into the CONNECT round trip, signals, and then goes silent
// forever — the R2-1 premise: the proxy accepted the connection and
// holds the handshake in flight without ever answering it.
type silentSocksStub struct {
	ln      net.Listener
	gotOnce chan struct{}
	once    sync.Once
	live    atomic.Int64
}

func startSilentSocksStub(t *testing.T) *silentSocksStub {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &silentSocksStub{ln: ln, gotOnce: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

func (s *silentSocksStub) addr() string { return s.ln.Addr().String() }

func (s *silentSocksStub) liveCount() int64 { return s.live.Load() }

func (s *silentSocksStub) waitGotGreeting(t *testing.T) {
	t.Helper()
	select {
	case <-s.gotOnce:
	case <-time.After(testAbortWait):
		t.Fatal("stub never observed the SOCKS5 greeting")
	}
}

func (s *silentSocksStub) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.live.Add(1)
		go s.handleConn(conn)
	}
}

// handleConn reads the greeting, signals, then blocks on reads until
// the connection ends, so liveCount observes client-side closes.
func (s *silentSocksStub) handleConn(conn net.Conn) {
	defer s.live.Add(-1)
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	var head [2]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return
	}
	if head[0] != 0x05 {
		return
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	// A valid no-auth selection: the real client now sends its CONNECT
	// request and blocks on the reply — the handshake is in flight.
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	s.once.Do(func() { close(s.gotOnce) })

	var sink [1]byte
	for {
		if _, err := conn.Read(sink[:]); err != nil {
			return
		}
	}
}

// TestSOCKS5HandshakeBoundedOnCancelledRequest proves the R2-1 fix on
// the proxy-only path: a request through BuildHTTPClient against a
// silent SOCKS5 proxy returns promptly when the request is cancelled,
// AND the inner SOCKS5 dial dies with it. The second half is the
// discriminator: net/http detaches the dial context from the request
// (getConn's context.WithoutCancel), so before the fix the handshake
// kept the stub's socket open indefinitely even though the request
// itself had long returned. Serial: shortens the handshake budget.
func TestSOCKS5HandshakeBoundedOnCancelledRequest(t *testing.T) {
	setSocksHandshakeTimeoutForTest(t, 250*time.Millisecond)

	stub := startSilentSocksStub(t)
	client, err := BuildHTTPClient(config.NetworkConfig{Proxy: "socks5://" + stub.addr()})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(client.CloseIdleConnections)

	ctx, cancel := context.WithCancel(context.Background())
	type requestResult struct {
		err error
	}
	result := make(chan requestResult, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://socks-silent.invalid:65535/", nil)
		if err != nil {
			result <- requestResult{err: err}
			return
		}
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		result <- requestResult{err: err}
	}()

	// Establish the premise: the handshake is genuinely in flight —
	// the stub received the greeting and answered method selection, so
	// the client is inside the CONNECT round trip. Not "the dial was
	// merely slow to connect".
	stub.waitGotGreeting(t)
	cancel()

	select {
	case res := <-result:
		require.Error(t, res.err, "cancelled request over a silent SOCKS5 proxy must fail")
	case <-time.After(testAbortWait):
		t.Fatal("cancelled request did not return within the bound")
	}

	// The inner SOCKS5 dial must die with the request: only the
	// dialer's own budget can close the socket, because net/http
	// detached the dial context from the cancellation. The stub's
	// connection ends only when the client side is really gone.
	require.Eventually(t, func() bool { return stub.liveCount() == 0 },
		5*time.Second, 50*time.Millisecond,
		"stub connection must be closed once the dialer's own budget aborts the handshake")
}

// TestDoHOverSocks5NestedDialBounded proves the R2-1 fix on the nested
// path: with DoHURL routed through the SOCKS5 proxy, the DoH lookup's
// own dial is a second, inner SOCKS5 dial inside the resolver's
// transport. The DoH client timeout bounds the lookup's return, but
// only the dialer's own budget closes the inner socket — before the
// fix it stayed open indefinitely after the lookup had already failed.
// The DoH endpoint is a .invalid hostname so the lookup cannot be
// skipped, and the target is too, so the outer dial needs it. Serial:
// shortens the handshake budget.
func TestDoHOverSocks5NestedDialBounded(t *testing.T) {
	setSocksHandshakeTimeoutForTest(t, 250*time.Millisecond)

	stub := startSilentSocksStub(t)
	cfg := config.NetworkConfig{
		Proxy:  "socks5://" + stub.addr(),
		DoHURL: "http://doh-silent.invalid:65535/dns-query",
	}
	client, err := BuildHTTPClient(cfg)
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(client.CloseIdleConnections)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	errCh := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://target-silent.invalid:65535/", nil)
		if err != nil {
			errCh <- err
			return
		}
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		errCh <- err
	}()

	// Premise: the inner nested dial is genuinely in flight — the stub
	// received the greeting of the DoH transport's SOCKS5 exchange and
	// answered method selection.
	stub.waitGotGreeting(t)
	cancel()

	select {
	case err := <-errCh:
		require.Error(t, err, "request needing a DoH lookup through a silent SOCKS5 proxy must fail")
		require.Less(t, time.Since(start), testAbortWait,
			"the nested SOCKS5 dial must be bounded by the dialer's own budget, not the DoH client timeout")
	case <-time.After(testAbortWait + time.Second):
		t.Fatal("request did not return within the bound")
	}

	require.Eventually(t, func() bool { return stub.liveCount() == 0 },
		5*time.Second, 50*time.Millisecond,
		"nested DoH-over-SOCKS5 dial must close the proxy socket")
}

// TestBuildTransportBoundedTimeouts pins the F4 transport defaults: a
// zero TLSHandshakeTimeout lets a stuck handshake hang a provider
// connection forever, and a zero IdleConnTimeout keeps keep-alive
// sockets open indefinitely. It also pins R6-1: the proxy-only HTTP
// transport must carry a bounded DialContext.
func TestBuildTransportBoundedTimeouts(t *testing.T) {
	t.Parallel()

	tr, err := BuildTransport(config.NetworkConfig{Proxy: "http://127.0.0.1:1"})
	require.NoError(t, err)
	require.NotNil(t, tr)
	require.Equal(t, 10*time.Second, tr.TLSHandshakeTimeout)
	require.Equal(t, 90*time.Second, tr.IdleConnTimeout)
	require.NotNil(t, tr.DialContext, "proxy-only HTTP transport must carry a bounded DialContext (R6-1)")
}

// keepAliveStub is an HTTP/1.1 server with keep-alive and no
// server-side idle deadline, tracking how many connections were
// accepted and how many are still open.
type keepAliveStub struct {
	ln       net.Listener
	srv      *http.Server
	mu       sync.Mutex
	accepted int
	live     int
}

func startKeepAliveStub(t *testing.T) *keepAliveStub {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &keepAliveStub{ln: ln}
	s.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, testTargetBody)
	})}
	t.Cleanup(func() { _ = s.srv.Close() })
	go func() { _ = s.srv.Serve(s) }()
	return s
}

func (s *keepAliveStub) Accept() (net.Conn, error) {
	conn, err := s.ln.Accept()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.accepted++
	s.live++
	s.mu.Unlock()
	return &trackedConn{Conn: conn, stub: s}, nil
}

func (s *keepAliveStub) Close() error { return s.ln.Close() }

func (s *keepAliveStub) Addr() net.Addr { return s.ln.Addr() }

func (s *keepAliveStub) acceptedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

func (s *keepAliveStub) liveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

// trackedConn decrements the stub's live count exactly once when the
// server closes the connection.
type trackedConn struct {
	net.Conn
	stub *keepAliveStub
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.stub.mu.Lock()
		c.stub.live--
		c.stub.mu.Unlock()
	})
	return c.Conn.Close()
}

// TestTransportCacheEvictionReleasesIdleConns proves the F5 fix: LRU
// eviction in a transport cache really closes the evicted entry's
// keep-alive pool — observable as the stub server's live connection
// count going back down and the next request re-dialing — not just as
// an in-memory cache size change. The test drives the exact production
// builder against its OWN isolated cache instance: the shared
// process-wide cache is fair game for other parallel tests'
// insertions, so an eviction there between the two warm-up requests is
// a legal schedule, not an oracle failure (audit R2-6).
func TestTransportCacheEvictionReleasesIdleConns(t *testing.T) {
	t.Parallel()

	stub := startKeepAliveStub(t)
	// The DoH URL is never queried: the request target below is an IP
	// literal, so the resolved dialer skips the resolver — but the
	// config still puts a resolver-mode transport plus its hidden DoH
	// transport into the cache entry.
	cfg := config.NetworkConfig{DoHURL: "http://127.0.0.1:65535/doh-never-queried"}
	cache := newTransportCache()
	tr, err := buildTransportWithCache(cfg, cache)
	require.NoError(t, err)
	require.NotNil(t, tr)
	client := &http.Client{Transport: tr}
	// A second client built from the SAME config must share the
	// cached transport, so the two warm-up requests pool onto one
	// connection — this is what makes the assertion below
	// discriminate cache reuse, not just one transport's keep-alive.
	tr2, err := buildTransportWithCache(cfg, cache)
	require.NoError(t, err)
	require.NotNil(t, tr2)
	require.True(t, tr == tr2, "same-config builds must share the cached transport")
	client2 := &http.Client{Transport: tr2}
	t.Cleanup(client.CloseIdleConnections)
	t.Cleanup(client2.CloseIdleConnections)

	stubURL := "http://" + stub.ln.Addr().String() + "/"
	fetch := func(c *http.Client) {
		ctx, cancel := context.WithTimeout(context.Background(), testAbortWait)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, stubURL, nil)
		require.NoError(t, err)
		resp, err := c.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, testTargetBody, string(body))
	}

	fetch(client)
	fetch(client2)
	base := stub.acceptedCount()
	require.Equal(t, 1, base, "two same-config clients must share the cached transport's one keep-alive connection")

	// Insert a full cache's worth of distinct configs so the entry
	// holding cfg's pooled connection is LRU-evicted; eviction closes
	// its idle connections.
	for i := 0; i < transportCacheCapacity+1; i++ {
		evictCfg := config.NetworkConfig{
			DoHURL: fmt.Sprintf("http://127.0.0.1:65535/evict-%d?nonce=%d", i, time.Now().UnixNano()),
		}
		tr, err := buildTransportWithCache(evictCfg, cache)
		require.NoError(t, err)
		require.NotNil(t, tr)
		t.Cleanup(tr.CloseIdleConnections)
	}

	// The pooled connection must be gone: the stub's live count drops
	// to zero — the server-side connection count going back DOWN.
	require.Eventually(t, func() bool { return stub.liveCount() == 0 },
		5*time.Second, 50*time.Millisecond,
		"evicted transport must release its idle connection")

	// The still-referenced clients cannot reuse the emptied pool: the
	// next request must open a new connection.
	fetch(client)
	require.Greater(t, stub.acceptedCount(), base, "request after eviction must re-dial")
}
