package nettransport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// resolveFunc resolves a hostname to its usable IP addresses — every
// A record in the server's answer order first, then the AAAA records
// as fallback.
type resolveFunc func(ctx context.Context, host string) ([]net.IP, error)

// buildResolver wires the custom resolver per the resolved config.
// Precedence: DoH endpoint first, then DNS-over-TCP through the proxy
// when both a DNS server and a proxy exist (UDP cannot tunnel through
// CONNECT-style proxies), then plain DNS over UDP, and finally no
// custom resolution at all (nil, nil, nil). The second return value
// lists the transports the resolver owns (the DoH endpoint's HTTP
// transport) so their keep-alive pools stay reachable for cleanup.
func buildResolver(rs resolved, proxyDial dialFunc) (resolveFunc, []*http.Transport, error) {
	switch {
	case rs.dohURL != "":
		fn, tr, err := newDoHResolver(rs.dohURL, rs.proxyURL, dohClientTimeout)
		if err != nil {
			return nil, nil, err
		}
		return fn, []*http.Transport{tr}, nil
	case rs.dnsServer != "" && proxyDial != nil:
		fn := dnsOverTCPResolver(rs.dnsServer, proxyDial)
		return fn, nil, nil
	case rs.dnsServer != "":
		fn := plainDNSResolver(rs.dnsServer)
		return fn, nil, nil
	default:
		return nil, nil, nil
	}
}

// plainDNSResolver resolves via an unproxied DNS server using Go's
// pure resolver with a custom Dial override (the same pattern as
// internal/dns/android.go), so the OS resolver setup is bypassed.
func plainDNSResolver(server string) resolveFunc {
	res := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, server)
		},
	}
	return func(ctx context.Context, host string) ([]net.IP, error) {
		addrs, err := res.LookupHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("lookup %q via DNS server %s: %w", host, server, err)
		}
		// Collect every parseable IPv4 address in the server's answer
		// order first, then every IPv6 one as fallback; unparseable
		// entries are skipped.
		var v4, v6 []net.IP
		for _, a := range addrs {
			ip := net.ParseIP(a)
			if ip == nil {
				continue
			}
			if ip.To4() != nil {
				v4 = append(v4, ip)
			} else {
				v6 = append(v6, ip)
			}
		}
		ips := append(v4, v6...)
		if len(ips) == 0 {
			return nil, fmt.Errorf(
				"no parseable IP address for %q from DNS server %s", host, server)
		}
		return ips, nil
	}
}

// packQuery builds a DNS wire-format query for host with the given
// record type. The transaction ID comes from crypto/rand, falling back
// to 0 on read failure — the ID is a spoofing hardening measure, not a
// correctness requirement for these stateless one-shot queries.
func packQuery(host string, qtype dnsmessage.Type) ([]byte, error) {
	var id uint16
	var idBuf [2]byte
	if _, err := rand.Read(idBuf[:]); err == nil {
		id = binary.BigEndian.Uint16(idBuf[:])
	}
	// dnsmessage.NewName requires the canonical form with a trailing
	// dot, so append one when the host has none.
	name := host
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	qname, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, fmt.Errorf("encode DNS name %q: %w", host, err)
	}
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:               id,
			RecursionDesired: true,
		},
		Questions: []dnsmessage.Question{
			{
				Name:  qname,
				Type:  qtype,
				Class: dnsmessage.ClassINET,
			},
		},
	}
	buf, err := msg.Pack()
	if err != nil {
		return nil, fmt.Errorf("pack DNS query for %q: %w", host, err)
	}
	return buf, nil
}

// answerIPs collects every A and AAAA answer record, in message
// order, converted to a net.IP; the bool reports whether the message
// contained at least one usable answer.
func answerIPs(msg *dnsmessage.Message) ([]net.IP, bool) {
	var ips []net.IP
	for _, ans := range msg.Answers {
		switch ans.Header.Type {
		case dnsmessage.TypeA:
			if a, ok := ans.Body.(*dnsmessage.AResource); ok {
				ips = append(ips, net.IP(a.A[:]))
			}
		case dnsmessage.TypeAAAA:
			if aaaa, ok := ans.Body.(*dnsmessage.AAAAResource); ok {
				ips = append(ips, net.IP(aaaa.AAAA[:]))
			}
		}
	}
	return ips, len(ips) > 0
}

// dohClientTimeout bounds every DoH exchange with a deadline the DoH
// client owns outright. Passing ctx into the request is not enough on
// its own: this resolver runs inside net/http's own dial machinery,
// which detaches the dial context from the request's cancellation (Go
// 1.26 transport.go getConn wraps it in context.WithoutCancel), so
// cancelling the outer HTTP request does not bound an in-flight lookup
// happening inside a dial.
const dohClientTimeout = 10 * time.Second

// newDoHResolver builds a resolver that queries an RFC 8484
// DNS-over-HTTPS endpoint. When proxyURL is non-nil the endpoint's
// client routes through that proxy: an HTTP proxy is set as
// http.Transport.Proxy, while a SOCKS5 proxy is tunneled with our own
// bounded socksDialer (see the comment at the call site), in both
// cases with the endpoint hostname resolved by the proxy itself.
// timeout is the per-exchange budget the client owns (tests pass a
// shorter one); the returned transport is owned by the resolver and
// must be tracked by the caller for keep-alive cleanup.
func newDoHResolver(endpoint string, proxyURL *url.URL, timeout time.Duration) (resolveFunc, *http.Transport, error) {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: transportTLSHandshakeTimeout,
		IdleConnTimeout:     transportIdleConnTimeout,
	}
	if proxyURL != nil {
		if proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h" {
			// Tunnel DoH through the SOCKS5 proxy with our own dialer
			// instead of tr.Proxy: net/http's built-in SOCKS5 handshake
			// (transport.go dialConn) runs under the detached dial
			// context with no deadline of its own, so the DoH client
			// timeout bounds client.Do's return but not the inner dial
			// — a silent proxy would leave that socket hanging forever
			// after the lookup had already failed. socksDialer's
			// synthesized deadline closes it. Endpoint hostnames still
			// reach the proxy unresolved (ATYP=0x03) and net/http still
			// layers TLS on top, so the wire behavior is identical.
			dial, err := socksDialer(proxyURL, socksHandshakeTimeout)
			if err != nil {
				return nil, nil, err
			}
			tr.DialContext = dial
		} else {
			tr.Proxy = http.ProxyURL(proxyURL)
		}
	}
	// Bounded initial TCP connect for the direct and HTTP-proxy
	// routes: the SOCKS5 route above already set its own bounded
	// dialer, but a nil DialContext would leave stdlib's zero dialer
	// (no timeout) governing the connect, and net/http detaches the
	// dial context from the DoH client's timeout, so dohClientTimeout
	// would not end a stuck dial.
	if tr.DialContext == nil {
		tr.DialContext = transportDialer.DialContext
	}
	client := &http.Client{Transport: tr, Timeout: timeout}
	resolve := func(ctx context.Context, host string) ([]net.IP, error) {
		// R4-2 (2026-09-22 round-8 audit): query BOTH families
		// unconditionally, not AAAA-only-when-A-is-empty. The DNS layer
		// resolving A does not prove those addresses are reachable — a
		// firewall or IPv4-only-blocked route can leave every A address
		// undialable while an AAAA address for the same name works fine,
		// and the resolver has no way to know that at resolution time.
		// The dialer (resolvedDialer/dialTarget in client.go) already
		// falls back through the full returned list, so combining both
		// families here is what actually makes that fallback reach a
		// working AAAA address. The client's keep-alive transport reuses
		// one connection for both requests, so this does not double the
		// proxy dial count the way a fresh-connection-per-query resolver
		// would.
		a, aErr := dohQuery(ctx, client, endpoint, host, dnsmessage.TypeA)
		aaaa, aaaaErr := dohQuery(ctx, client, endpoint, host, dnsmessage.TypeAAAA)
		ips := append(a, aaaa...)
		if len(ips) > 0 {
			return ips, nil
		}
		if aErr != nil {
			return nil, aErr
		}
		if aaaaErr != nil {
			return nil, aaaaErr
		}
		return nil, fmt.Errorf(
			"no A/AAAA answer for %q from DoH endpoint %s", host, redactedURL(endpoint))
	}
	return resolve, tr, nil
}

// dohQuery performs one RFC 8484 query against the endpoint. POST is
// used instead of GET because it sends the wire-format query as the
// request body, avoiding the base64url query-parameter encoding —
// RFC 8484 section 4.1 allows both forms.
func dohQuery(ctx context.Context, client *http.Client, endpoint, host string,
	qtype dnsmessage.Type,
) ([]net.IP, error) {
	query, err := packQuery(host, qtype)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint, bytes.NewReader(query))
	if err != nil {
		return nil, fmt.Errorf("build DoH request for %q: %w", host, err)
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := client.Do(req)
	if err != nil {
		// R2-5 residual (2026-09-22 round-8 audit): net/http returns a
		// *url.Error here, and its own Error() string embeds the raw
		// endpoint URL with the query string intact (only userinfo is
		// stripped by the stdlib) -- a secret in the DoH endpoint's query
		// would survive %w's formatting even though %s above already
		// uses the safe redacted form. urlErrReason unwraps to the
		// underlying cause (e.g. a dial/timeout error), which does not
		// carry the endpoint URL, while preserving it for errors.Is/As.
		return nil, fmt.Errorf("DoH request to %s for %q: %w",
			redactedURL(endpoint), host, urlErrReason(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"DoH endpoint %s returned %s for %q", redactedURL(endpoint), resp.Status, host)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/dns-message") {
		return nil, fmt.Errorf(
			"DoH endpoint %s returned unexpected Content-Type %q", redactedURL(endpoint), ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("read DoH response from %s: %w", redactedURL(endpoint), err)
	}
	var msg dnsmessage.Message
	if err := msg.Unpack(body); err != nil {
		return nil, fmt.Errorf("unpack DoH response from %s: %w", redactedURL(endpoint), err)
	}
	if msg.RCode != dnsmessage.RCodeSuccess {
		return nil, fmt.Errorf(
			"DoH endpoint %s returned rcode %s for %q", redactedURL(endpoint), msg.RCode, host)
	}
	if ips, ok := answerIPs(&msg); ok {
		return ips, nil
	}
	return nil, nil // NOERROR but no usable answer record.
}

// dnsOverTCPResolver resolves via plain DNS over TCP, in the combined
// proxy + plain-DNS mode: the DNS connection itself is opened through
// proxyDial, so the lookup flows through the proxy tunnel. TCP is
// required because UDP cannot tunnel through CONNECT-style proxies,
// with RFC 1035 section 4.2.2 framing (a 2-byte big-endian length
// prefix before each message). Each query opens its own connection —
// simple and correct, with no pipelining; both A and AAAA are queried
// unconditionally (R4-2, 2026-09-22 round-8 audit — see newDoHResolver's
// doc for why "A succeeded" does not mean "A is dialable"), so a lookup
// now opens two connections instead of one.
func dnsOverTCPResolver(server string, proxyDial dialFunc) resolveFunc {
	return func(ctx context.Context, host string) ([]net.IP, error) {
		a, aErr := dnsTCPQuery(ctx, server, proxyDial, host, dnsmessage.TypeA)
		aaaa, aaaaErr := dnsTCPQuery(ctx, server, proxyDial, host, dnsmessage.TypeAAAA)
		ips := append(a, aaaa...)
		if len(ips) > 0 {
			return ips, nil
		}
		if aErr != nil {
			return nil, aErr
		}
		if aaaaErr != nil {
			return nil, aaaaErr
		}
		return nil, fmt.Errorf(
			"no A/AAAA answer for %q from DNS server %s", host, server)
	}
}

// dnsTCPQuery performs one DNS-over-TCP query over a fresh proxied
// connection, closing it afterwards.
func dnsTCPQuery(ctx context.Context, server string, proxyDial dialFunc,
	host string, qtype dnsmessage.Type,
) ([]net.IP, error) {
	query, err := packQuery(host, qtype)
	if err != nil {
		return nil, err
	}
	conn, err := proxyDial(ctx, "tcp", server)
	if err != nil {
		return nil, fmt.Errorf("dial DNS server %s through proxy: %w", server, err)
	}
	defer func() { _ = conn.Close() }()

	// The write/read sequence below is plain blocking I/O with no
	// context parameter, so bound it like the CONNECT handshake: a
	// silent DNS server (or a tunnel that stalls carrying the query)
	// must not hold the lookup open past its budget or past the
	// caller's cancellation. The connection is closed on return, so
	// the guard never transfers ownership anywhere.
	guard := armHandshakeGuard(ctx, conn, dnsQueryTimeout)
	defer func() { _ = guard.stop() }()

	// RFC 1035 section 4.2.2: prefix the message with its 2-byte
	// big-endian length.
	frame := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(query)))
	copy(frame[2:], query)
	if _, err := conn.Write(frame); err != nil {
		return nil, fmt.Errorf("send DNS query to %s: %w", server, err)
	}

	var prefix [2]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		return nil, fmt.Errorf("read DNS response length from %s: %w", server, err)
	}
	length := int(binary.BigEndian.Uint16(prefix[:]))
	// Sanity cap: a 2-byte prefix can express at most 64KB-1, and RFC
	// 1035 keeps DNS-over-TCP messages under 64KB; reject anything else
	// up front instead of allocating for it.
	if length > 64<<10 {
		return nil, fmt.Errorf("DNS response from %s exceeds 64KB (%d bytes)", server, length)
	}
	msgBuf := make([]byte, length)
	if _, err := io.ReadFull(conn, msgBuf); err != nil {
		return nil, fmt.Errorf("read DNS response from %s: %w", server, err)
	}

	var msg dnsmessage.Message
	if err := msg.Unpack(msgBuf); err != nil {
		return nil, fmt.Errorf("unpack DNS response from %s: %w", server, err)
	}
	if msg.RCode != dnsmessage.RCodeSuccess {
		return nil, fmt.Errorf(
			"DNS server %s returned rcode %s for %q", server, msg.RCode, host)
	}
	if ips, ok := answerIPs(&msg); ok {
		return ips, nil
	}
	return nil, nil // NOERROR but no usable answer record.
}
