package nettransport

import (
	"crypto/tls"
	"net/http"
	"strconv"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// fakeHostnames below all use the reserved .invalid TLD (RFC 2606),
// which no real resolver can ever answer; any OS-resolver false pass
// is therefore impossible — only the configured proxy/resolver can
// make these requests reach the local target servers.

func TestBuildHTTPClientZeroConfig(t *testing.T) {
	t.Parallel()

	c, err := BuildHTTPClient(config.NetworkConfig{})
	require.NoError(t, err)
	require.Nil(t, c, "zero config must keep the caller's own client (nil, nil)")

	tr, err := BuildTransport(config.NetworkConfig{})
	require.NoError(t, err)
	require.Nil(t, tr, "zero config must keep the caller's own transport (nil, nil)")
}

func TestHTTPProxyOnlyMode(t *testing.T) {
	t.Parallel()

	t.Run("plain http absolute form", func(t *testing.T) {
		t.Parallel()
		_, targetHostport := startTargetServer(t)
		targetPort := requirePort(t, targetHostport)
		proxy := startHTTPProxy(t, targetHostport)
		client := buildClient(t, config.NetworkConfig{Proxy: proxy.URL()})

		requireBody(t, client,
			"http://proxy-resolved.invalid:"+strconv.Itoa(targetPort)+"/",
			testTargetBody)

		// Exactly the fake authority reached the proxy UNRESOLVED: the
		// .invalid name cannot have been resolved by any resolver, so
		// this proves the stdlib forwarded it for proxy-side handling.
		require.Equal(t, []string{"proxy-resolved.invalid:" + strconv.Itoa(targetPort)}, proxy.seen())
	})

	t.Run("connect tls tunnel", func(t *testing.T) {
		t.Parallel()
		_, targetHostport := startTLSTargetServer(t)
		proxy := startHTTPProxy(t, targetHostport)
		client := buildClient(t, config.NetworkConfig{Proxy: proxy.URL()})

		// Composition check: callers can rewrap the built transport.
		// httptest ships a self-signed cert, so skipping verification is
		// a test-side concern only.
		client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // self-signed httptest cert.
		}

		requireBody(t, client,
			"https://connect-resolved.invalid:"+strconv.Itoa(requirePort(t, targetHostport))+"/",
			testTargetBody)

		require.Equal(t, []string{"connect-resolved.invalid:" + strconv.Itoa(requirePort(t, targetHostport))}, proxy.seen())
	})
}

func TestSOCKS5ProxyOnlyMode(t *testing.T) {
	t.Parallel()

	t.Run("domain reaches server unresolved", func(t *testing.T) {
		t.Parallel()
		_, targetHostport := startTargetServer(t)
		proxy := startSOCKS5Proxy(t, "", "", targetHostport)
		client := buildClient(t, config.NetworkConfig{Proxy: "socks5://" + proxy.addr()})

		requireBody(t, client,
			"http://socks-resolved.invalid:"+strconv.Itoa(requirePort(t, targetHostport))+"/",
			testTargetBody)

		// THE LOAD-BEARING CLAIM: exactly one CONNECT reached the SOCKS5
		// server, carrying the raw DOMAIN (ATYP 0x03) — the name was
		// never resolved client-side, so remote (proxy-side) resolution
		// really happened and the .invalid name cannot have leaked to
		// any real resolver.
		require.Equal(t, []socksRequestDouble{{
			Atyp: 0x03,
			Addr: "socks-resolved.invalid",
			Port: requirePort(t, targetHostport),
		}}, proxy.requests())
	})

	t.Run("username password auth succeeds", func(t *testing.T) {
		t.Parallel()
		_, targetHostport := startTargetServer(t)
		proxy := startSOCKS5Proxy(t, "u", "p", targetHostport)
		client := buildClient(t, config.NetworkConfig{Proxy: "socks5://u:p@" + proxy.addr()})

		requireBody(t, client,
			"http://socks-resolved.invalid:"+strconv.Itoa(requirePort(t, targetHostport))+"/",
			testTargetBody)
		require.Equal(t, []socksRequestDouble{{
			Atyp: 0x03,
			Addr: "socks-resolved.invalid",
			Port: requirePort(t, targetHostport),
		}}, proxy.requests())
		require.Equal(t, 0, proxy.authFailures())
	})

	t.Run("wrong password fails", func(t *testing.T) {
		t.Parallel()
		_, targetHostport := startTargetServer(t)
		proxy := startSOCKS5Proxy(t, "u", "p", targetHostport)
		client := buildClient(t, config.NetworkConfig{Proxy: "socks5://u:wrong@" + proxy.addr()})

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			"http://socks-resolved.invalid:"+strconv.Itoa(requirePort(t, targetHostport))+"/", nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.Error(t, err, "SOCKS5 auth failure must fail the request")
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.GreaterOrEqual(t, proxy.authFailures(), 1)
	})
}

func TestCombinedPlainDNSThroughProxyMode(t *testing.T) {
	t.Parallel()

	// Both subtests share the DNS double; UDP is never touched in this
	// mode (UDP cannot tunnel through the proxy), so per-subtest
	// assertions stay independent.
	dnsSrv := startDNSServer(t, loopbackIP)
	dnsPort := requirePort(t, dnsSrv.tcpAddr())
	_, targetHostport := startTargetServer(t)
	targetPort := requirePort(t, targetHostport)
	requestURL := "http://tunneled-dns.invalid:" + strconv.Itoa(targetPort) + "/"

	t.Run("socks5", func(t *testing.T) {
		t.Parallel()
		proxy := startSOCKS5Proxy(t, "", "", targetHostport)
		cfg := config.NetworkConfig{
			Proxy:     "socks5://" + proxy.addr(),
			DNSServer: dnsSrv.tcpAddr(),
		}
		client := buildClient(t, cfg)

		requireBody(t, client, requestURL, testTargetBody)

		// (a) Plain DNS went over TCP through the proxy; UDP is
		// unreachable through a CONNECT-style tunnel.
		require.GreaterOrEqual(t, dnsSrv.hitsTCP(), 1)
		require.Equal(t, 0, dnsSrv.hitsUDP())
		// (b) The fake name really reached our DNS server.
		require.Contains(t, dnsSrv.names(), "tunneled-dns.invalid")
		// (c)+(d) Exactly three dials traversed the proxy, IN ORDER: the
		// DNS-over-TCP resolver's A query, then its AAAA query (R4-2,
		// 2026-09-22 round-8 audit — both families are queried
		// unconditionally now, each opening its own fresh connection),
		// then the final connection to the RESOLVED 127.0.0.1 target —
		// the hostname was never re-sent to the proxy.
		require.Equal(t, []socksRequestDouble{
			{Atyp: 0x01, Addr: "127.0.0.1", Port: dnsPort},
			{Atyp: 0x01, Addr: "127.0.0.1", Port: dnsPort},
			{Atyp: 0x01, Addr: "127.0.0.1", Port: targetPort},
		}, proxy.requests())
		// (e) The chain delivered the real body.
	})

	t.Run("http", func(t *testing.T) {
		t.Parallel()
		proxy := startHTTPProxy(t, targetHostport)
		cfg := config.NetworkConfig{
			Proxy:     proxy.URL(),
			DNSServer: dnsSrv.tcpAddr(),
		}
		client := buildClient(t, cfg)

		requireBody(t, client, requestURL, testTargetBody)

		require.GreaterOrEqual(t, dnsSrv.hitsTCP(), 1)
		require.Equal(t, 0, dnsSrv.hitsUDP())
		require.Contains(t, dnsSrv.names(), "tunneled-dns.invalid")
		// CONNECT authority-form order: the DNS-over-TCP resolver's A
		// query, then its AAAA query (R4-2, each opens its own fresh
		// connection), then the resolved-IP target connection.
		require.Equal(t, []string{dnsSrv.tcpAddr(), dnsSrv.tcpAddr(), targetHostport}, proxy.seen())
	})
}

func TestCombinedDoHThroughProxyMode(t *testing.T) {
	t.Parallel()

	_, targetHostport := startTargetServer(t)
	targetPort := requirePort(t, targetHostport)
	requestURL := "http://tunneled-doh.invalid:" + strconv.Itoa(targetPort) + "/"

	t.Run("socks5", func(t *testing.T) {
		t.Parallel()
		doh := startDoHServer(t, loopbackIP)
		dohPort := requirePort(t, hostOnlyOfURL(t, doh.URL()))
		proxy := startSOCKS5Proxy(t, "", "", targetHostport)
		cfg := config.NetworkConfig{
			Proxy:  "socks5://" + proxy.addr(),
			DoHURL: doh.URL(),
		}
		client := buildClient(t, cfg)

		requireBody(t, client, requestURL, testTargetBody)

		// The lookup really went through our DoH endpoint.
		require.GreaterOrEqual(t, doh.hits(), 1)
		require.Contains(t, doh.names(), "tunneled-doh.invalid")
		// DoH query and final connection both traversed the proxy, as
		// two IP-literal dials in order; the hostname itself never did.
		// Still two dials despite R4-2 querying both A and AAAA now: the
		// DoH client's http.Transport keeps the SOCKS5-tunneled
		// connection alive and reuses it for the second (AAAA) request,
		// so only one SOCKS5 CONNECT is ever issued for the lookup —
		// unlike the HTTP-proxy subtest below, where the proxy double
		// records one entry per absolute-form request regardless of
		// connection reuse.
		require.Equal(t, []socksRequestDouble{
			{Atyp: 0x01, Addr: "127.0.0.1", Port: dohPort},
			{Atyp: 0x01, Addr: "127.0.0.1", Port: targetPort},
		}, proxy.requests())
	})

	t.Run("http", func(t *testing.T) {
		t.Parallel()
		doh := startDoHServer(t, loopbackIP)
		proxy := startHTTPProxy(t, targetHostport)
		cfg := config.NetworkConfig{
			Proxy:  proxy.URL(),
			DoHURL: doh.URL(),
		}
		client := buildClient(t, cfg)

		requireBody(t, client, requestURL, testTargetBody)

		require.GreaterOrEqual(t, doh.hits(), 1)
		require.Contains(t, doh.names(), "tunneled-doh.invalid")
		// The inner DoH client's absolute-form requests hit the proxy
		// first — the A query, then the AAAA query (R4-2: both families
		// queried unconditionally; unlike the SOCKS5 subtest above, the
		// proxy double records each absolute-form request regardless of
		// TCP keep-alive reuse) — then the outer CONNECT to the resolved
		// IP.
		require.Equal(t, []string{hostOnlyOfURL(t, doh.URL()), hostOnlyOfURL(t, doh.URL()), targetHostport}, proxy.seen())
	})
}
