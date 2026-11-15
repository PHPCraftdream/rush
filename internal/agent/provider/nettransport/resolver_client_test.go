package nettransport

import (
	"strconv"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestPlainDNSOnlyMode proves resolver-only plain DNS: the request to
// the fake .invalid name can only succeed if the configured DNS
// server really answered it, because no OS resolver can resolve
// .invalid (RFC 2606).
func TestPlainDNSOnlyMode(t *testing.T) {
	t.Parallel()

	dnsSrv := startDNSServer(t, loopbackIP)
	_, targetHostport := startTargetServer(t)
	client := buildClient(t, config.NetworkConfig{DNSServer: dnsSrv.udpAddr()})

	requireBody(t, client,
		"http://plain-dns.invalid:"+strconv.Itoa(requirePort(t, targetHostport))+"/",
		testTargetBody)

	require.GreaterOrEqual(t, dnsSrv.totalHits(), 1)
	require.Contains(t, dnsSrv.names(), "plain-dns.invalid")
}

// TestDoHOnlyMode proves resolver-only DoH plus the documented
// DoH-beats-DNS precedence, both observable on the wire.
func TestDoHOnlyMode(t *testing.T) {
	t.Parallel()

	doh := startDoHServer(t, loopbackIP)
	_, targetHostport := startTargetServer(t)
	client := buildClient(t, config.NetworkConfig{DoHURL: doh.URL()})

	requireBody(t, client,
		"http://doh-resolved.invalid:"+strconv.Itoa(requirePort(t, targetHostport))+"/",
		testTargetBody)

	require.GreaterOrEqual(t, doh.hits(), 1)
	require.Contains(t, doh.names(), "doh-resolved.invalid")

	t.Run("doh wins over dns server", func(t *testing.T) {
		t.Parallel()
		dohBoth := startDoHServer(t, loopbackIP)
		dnsSrv := startDNSServer(t, loopbackIP)
		cfg := config.NetworkConfig{
			DoHURL:    dohBoth.URL(),
			DNSServer: dnsSrv.udpAddr(),
		}
		client := buildClient(t, cfg)

		requireBody(t, client,
			"http://doh-resolved.invalid:"+strconv.Itoa(requirePort(t, targetHostport))+"/",
			testTargetBody)

		require.GreaterOrEqual(t, dohBoth.hits(), 1)
		// Precedence is observable: with DoH configured the plain DNS
		// server must never be queried at all.
		require.Equal(t, 0, dnsSrv.totalHits())
	})
}

// TestBuildHTTPClientResolverOnlyDialsResolvedIP needs no extra
// machinery: TestPlainDNSOnlyMode and TestDoHOnlyMode already prove
// it on the wire — the .invalid hostname can only reach the local
// target when the client dials the resolver-provided 127.0.0.1
// answer, and the fixed body matches on every run.
