package nettransport

import (
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

// testClientTimeout bounds every request made through test-built
// clients so a stuck double can never hang a test run.
const testClientTimeout = 10 * time.Second

// testTargetBody is the fixed payload served by every target server;
// matching it proves the response really traversed the whole chain.
const testTargetBody = "hello from target"

// loopbackIP is both the bind address for every double and the fake
// answer the DNS/DoH doubles hand out, which steers resolved dials
// back to the local target servers.
var loopbackIP = net.IPv4(127, 0, 0, 1).To4()

// buildClient wraps BuildHTTPClient with the shared 10-second timeout
// and idle-connection cleanup.
func buildClient(t *testing.T, cfg config.NetworkConfig) *http.Client {
	t.Helper()
	c, err := BuildHTTPClient(cfg)
	require.NoError(t, err)
	require.NotNil(t, c)
	c.Timeout = testClientTimeout
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// targetHandler answers every request with the fixed test body.
func targetHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, testTargetBody)
	})
}

// startTargetServer starts a plain HTTP server on loopback answering
// every request with the fixed test body, and returns it together
// with its 127.0.0.1:port hostport.
func startTargetServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewServer(targetHandler())
	t.Cleanup(ts.Close)
	return ts, hostOnlyOfURL(t, ts.URL)
}

// startTLSTargetServer is startTargetServer over TLS; the server uses
// httptest's self-signed certificate.
func startTLSTargetServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewTLSServer(targetHandler())
	t.Cleanup(ts.Close)
	return ts, hostOnlyOfURL(t, ts.URL)
}

// hostOnlyOfURL parses a raw URL and returns its host:port authority.
func hostOnlyOfURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u.Host
}

// requireBody fetches rawURL through client and requires a 200
// response whose body matches wantBody exactly.
func requireBody(t *testing.T, client *http.Client, rawURL, wantBody string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, wantBody, string(body))
}

// requirePort parses a 127.0.0.1:port hostport and returns the
// numeric port.
func requirePort(t *testing.T, hostport string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(hostport)
	require.NoError(t, err)
	n, err := strconv.Atoi(port)
	require.NoError(t, err)
	return n
}

// isIPLiteral reports whether host is an IP address literal.
func isIPLiteral(host string) bool {
	return net.ParseIP(host) != nil
}

// hostOnly returns the host part of a host:port authority, or the
// input unchanged when it carries no port.
func hostOnly(authority string) string {
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		return authority
	}
	return host
}

// buildDNSResponse echoes the query ID and Question section, flags
// the message as a successful response, and appends one A or AAAA
// record for answerIP when the first question asks for one; other
// question types get an empty NOERROR.
func buildDNSResponse(query *dnsmessage.Message, answerIP net.IP) ([]byte, error) {
	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 query.ID,
			Response:           true,
			Authoritative:      true,
			RecursionDesired:   query.RecursionDesired,
			RecursionAvailable: true,
			RCode:              dnsmessage.RCodeSuccess,
		},
		Questions: query.Questions,
	}
	if len(query.Questions) > 0 {
		q := query.Questions[0]
		switch q.Type {
		case dnsmessage.TypeA:
			var a [4]byte
			copy(a[:], answerIP.To4())
			resp.Answers = append(resp.Answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{
					Name:  q.Name,
					Type:  dnsmessage.TypeA,
					Class: dnsmessage.ClassINET,
					TTL:   60,
				},
				Body: &dnsmessage.AResource{A: a},
			})
		case dnsmessage.TypeAAAA:
			var aaaa [16]byte
			copy(aaaa[:], answerIP.To16())
			resp.Answers = append(resp.Answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{
					Name:  q.Name,
					Type:  dnsmessage.TypeAAAA,
					Class: dnsmessage.ClassINET,
					TTL:   60,
				},
				Body: &dnsmessage.AAAAResource{AAAA: aaaa},
			})
		}
	}
	return resp.Pack()
}

// dnsQueryName returns the first question name of a wire-format DNS
// message with its trailing dot stripped, plus whether unpacking
// succeeded.
func dnsQueryName(wire []byte) (string, *dnsmessage.Message, bool) {
	var query dnsmessage.Message
	if err := query.Unpack(wire); err != nil {
		return "", nil, false
	}
	name := ""
	if len(query.Questions) > 0 {
		name = strings.TrimSuffix(query.Questions[0].Name.String(), ".")
	}
	return name, &query, true
}

// httpProxyDouble is a real HTTP forward proxy on loopback. It
// records the authority of every request it sees, in arrival order.
// Authorities that are not IP literals are "resolved" proxy-side by
// dialing the fixed fallbackTarget, which models a proxy performing
// its own name resolution with zero OS involvement in the test
// process.
type httpProxyDouble struct {
	ts          *httptest.Server
	fallback    string
	mu          sync.Mutex
	authorities []string
}

// startHTTPProxy starts the proxy double and registers its cleanup.
func startHTTPProxy(t *testing.T, fallbackTarget string) *httpProxyDouble {
	t.Helper()
	p := &httpProxyDouble{fallback: fallbackTarget}
	p.ts = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.ts.Close)
	return p
}

// URL returns the proxy's http://127.0.0.1:port base URL.
func (p *httpProxyDouble) URL() string { return p.ts.URL }

// seen returns a snapshot of recorded authorities in arrival order;
// comparing the whole slice doubles as the ordering check.
func (p *httpProxyDouble) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.authorities...)
}

// record appends one authority under the mutex.
func (p *httpProxyDouble) record(authority string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authorities = append(p.authorities, authority)
}

// handle dispatches CONNECT tunneling and plain forward proxying,
// recording the request authority either way.
func (p *httpProxyDouble) handle(w http.ResponseWriter, r *http.Request) {
	p.record(r.Host)
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleForward(w, r)
}

// handleConnect hijacks the connection for an authority-form CONNECT
// request, answers 200 raw, dials the upstream, and relays bytes in
// both directions.
func (p *httpProxyDouble) handleConnect(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = clientConn.Close() }()

	// Proxy-side "resolution": IP literals are dialed as-is, names go
	// to the fixed fallback target (a .invalid name can never be
	// resolved here, so a false pass is impossible).
	upstreamAddr := p.fallback
	if host := hostOnly(r.Host); isIPLiteral(host) {
		upstreamAddr = r.Host
	}
	upstream, err := net.DialTimeout("tcp", upstreamAddr, testClientTimeout)
	if err != nil {
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer func() { _ = upstream.Close() }()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	go func() {
		_, _ = io.Copy(upstream, clientConn)
		_ = upstream.Close()
		_ = clientConn.Close()
	}()
	_, _ = io.Copy(clientConn, upstream)
	_ = upstream.Close()
	_ = clientConn.Close()
}

// hopByHopHeaders are stripped from relayed forward-proxy responses.
var hopByHopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive"}

// handleForward relays an absolute-form proxy request. Names are
// "resolved" proxy-side by rewriting the URL host to the fallback
// target; IP literals are forwarded unchanged.
func (p *httpProxyDouble) handleForward(w http.ResponseWriter, r *http.Request) {
	// Clone shares Body; RoundTrip consumes it exactly once.
	out := r.Clone(r.Context())
	// Server-received requests carry RequestURI, which client
	// transports reject; clear it before re-sending.
	out.RequestURI = ""
	if !isIPLiteral(hostOnly(r.URL.Host)) {
		out.URL.Host = p.fallback
	}
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, hop := range hopByHopHeaders {
		resp.Header.Del(hop)
	}
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// socksRequestDouble is one recorded SOCKS5 CONNECT request: the
// address type byte, the address exactly as the client sent it (for
// ATYP 0x03 the raw, unresolved domain name), and the port.
type socksRequestDouble struct {
	Atyp byte
	Addr string
	Port int
}

// socksProxyDouble is a hand-rolled RFC 1928 SOCKS5 server on a raw
// loopback listener. ATYP 0x03 addresses are "resolved" server-side
// by dialing fallbackTarget when set; without a fallback the raw
// domain is dialed (which must fail for .invalid names).
type socksProxyDouble struct {
	ln        net.Listener
	user      string
	pass      string
	fallback  string
	mu        sync.Mutex
	requests_ []socksRequestDouble
	authFails int
}

// startSOCKS5Proxy starts the SOCKS5 double and registers its
// cleanup. An empty user means the no-auth method is offered.
func startSOCKS5Proxy(t *testing.T, user, pass, fallbackTarget string) *socksProxyDouble {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &socksProxyDouble{ln: ln, user: user, pass: pass, fallback: fallbackTarget}
	t.Cleanup(func() { _ = ln.Close() })
	go p.serve()
	return p
}

// addr returns the proxy's 127.0.0.1:port listen address.
func (p *socksProxyDouble) addr() string { return p.ln.Addr().String() }

// requests returns a snapshot of recorded CONNECT requests in order.
func (p *socksProxyDouble) requests() []socksRequestDouble {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]socksRequestDouble(nil), p.requests_...)
}

// authFailures reports how many username/password attempts failed.
func (p *socksProxyDouble) authFailures() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.authFails
}

// record appends one CONNECT request under the mutex.
func (p *socksProxyDouble) record(req socksRequestDouble) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests_ = append(p.requests_, req)
}

// recordAuthFailure counts one failed username/password attempt.
func (p *socksProxyDouble) recordAuthFailure() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authFails++
}

// serve accepts connections until the listener is closed.
func (p *socksProxyDouble) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handleConn(conn)
	}
}

// socksAuthNone and socksAuthUserPass are the RFC 1928 method bytes.
const (
	socksAuthNone     = 0x00
	socksAuthUserPass = 0x02
)

// socksRep values used by the double.
const (
	socksRepSuccess             = 0x00
	socksRepGeneralFailure      = 0x01
	socksRepCommandNotSupported = 0x07
	socksRepNoAcceptableMethods = 0xFF
)

// socksReply writes a bare reply frame with a zeroed IPv4 bound
// address (sufficient for CONNECT responses to the x/net client).
func socksReply(conn net.Conn, rep byte) error {
	_, err := conn.Write([]byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err
}

// handleConn runs one client connection through greeting, optional
// username/password auth, request parsing, and bidirectional relay.
func (p *socksProxyDouble) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	// Bound the handshake only; deadlines are cleared before relaying.
	_ = conn.SetDeadline(time.Now().Add(testClientTimeout))

	// Greeting: VER + NMETHODS + METHODS.
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
	chosen := byte(socksAuthNone)
	if p.user != "" {
		chosen = socksAuthUserPass
	}
	offered := false
	for _, m := range methods {
		if m == chosen {
			offered = true
			break
		}
	}
	if !offered {
		_, _ = conn.Write([]byte{0x05, socksRepNoAcceptableMethods})
		return
	}
	if _, err := conn.Write([]byte{0x05, chosen}); err != nil {
		return
	}

	if chosen == socksAuthUserPass {
		// RFC 1929: VER, ULEN, UNAME, PLEN, PASSWD.
		var vh [2]byte
		if _, err := io.ReadFull(conn, vh[:]); err != nil {
			return
		}
		if vh[0] != 0x01 {
			return
		}
		uname := make([]byte, vh[1])
		if _, err := io.ReadFull(conn, uname); err != nil {
			return
		}
		var plen [1]byte
		if _, err := io.ReadFull(conn, plen[:]); err != nil {
			return
		}
		passwd := make([]byte, plen[0])
		if _, err := io.ReadFull(conn, passwd); err != nil {
			return
		}
		if string(uname) != p.user || string(passwd) != p.pass {
			p.recordAuthFailure()
			_, _ = conn.Write([]byte{0x01, 0x01})
			return
		}
		if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
			return
		}
	}

	// Request: VER, CMD, RSV, ATYP, ADDR, PORT.
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return
	}
	if hdr[0] != 0x05 {
		return
	}
	atyp := hdr[3]
	var addr string
	switch atyp {
	case 0x01:
		var b [4]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return
		}
		addr = net.IP(b[:]).String()
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return
		}
		name := make([]byte, l[0])
		if _, err := io.ReadFull(conn, name); err != nil {
			return
		}
		addr = string(name)
	case 0x04:
		var b [16]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return
		}
		addr = net.IP(b[:]).String()
	default:
		_ = socksReply(conn, socksRepGeneralFailure)
		return
	}
	var portB [2]byte
	if _, err := io.ReadFull(conn, portB[:]); err != nil {
		return
	}
	port := int(binary.BigEndian.Uint16(portB[:]))
	p.record(socksRequestDouble{Atyp: atyp, Addr: addr, Port: port})

	if hdr[1] != 0x01 { // Only CMD=CONNECT is supported.
		_ = socksReply(conn, socksRepCommandNotSupported)
		return
	}

	// Proxy-side "resolution": IP literals dial themselves, names dial
	// the fallback target when set (else the raw domain, which fails
	// for .invalid names).
	upstreamAddr := p.fallback
	switch {
	case atyp == 0x01 || atyp == 0x04:
		upstreamAddr = net.JoinHostPort(addr, strconv.Itoa(port))
	case p.fallback == "":
		upstreamAddr = net.JoinHostPort(addr, strconv.Itoa(port))
	}
	upstream, err := net.DialTimeout("tcp", upstreamAddr, testClientTimeout)
	if err != nil {
		_ = socksReply(conn, socksRepGeneralFailure)
		return
	}
	defer func() { _ = upstream.Close() }()
	if err := socksReply(conn, socksRepSuccess); err != nil {
		return
	}

	_ = conn.SetDeadline(time.Time{})
	go func() {
		_, _ = io.Copy(upstream, conn)
		_ = upstream.Close()
		_ = conn.Close()
	}()
	_, _ = io.Copy(conn, upstream)
	_ = upstream.Close()
	_ = conn.Close()
}

// dnsServerDouble serves wire-format DNS on BOTH UDP and TCP on
// loopback, answering every A/AAAA question with a single A or AAAA
// record for answerIP, and recording which transport answered.
type dnsServerDouble struct {
	udp      *net.UDPConn
	tcp      net.Listener
	answerIP net.IP

	mu      sync.Mutex
	udpHits int
	tcpHits int
	asked   []string
}

// startDNSServer starts both transports and registers their cleanup.
func startDNSServer(t *testing.T, answerIP net.IP) *dnsServerDouble {
	t.Helper()
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	d := &dnsServerDouble{udp: udpConn, tcp: tcpLn, answerIP: answerIP}
	t.Cleanup(func() {
		_ = udpConn.Close()
		_ = tcpLn.Close()
	})
	go d.serveUDP()
	go d.serveTCP()
	return d
}

// udpAddr returns the UDP transport's 127.0.0.1:port.
func (d *dnsServerDouble) udpAddr() string { return d.udp.LocalAddr().String() }

// tcpAddr returns the TCP transport's 127.0.0.1:port.
func (d *dnsServerDouble) tcpAddr() string { return d.tcp.Addr().String() }

// hitsUDP reports how many queries were answered over UDP.
func (d *dnsServerDouble) hitsUDP() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.udpHits
}

// hitsTCP reports how many queries were answered over TCP.
func (d *dnsServerDouble) hitsTCP() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tcpHits
}

// totalHits reports queries answered over both transports.
func (d *dnsServerDouble) totalHits() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.udpHits + d.tcpHits
}

// names returns a snapshot of recorded question names in order.
func (d *dnsServerDouble) names() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.asked...)
}

// recordQuery counts one answered query on the given transport.
func (d *dnsServerDouble) recordQuery(transport, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if transport == "udp" {
		d.udpHits++
	} else {
		d.tcpHits++
	}
	d.asked = append(d.asked, name)
}

// serveUDP answers one datagram per ReadFromUDP until close.
func (d *dnsServerDouble) serveUDP() {
	buf := make([]byte, 64<<10)
	for {
		n, from, err := d.udp.ReadFromUDP(buf)
		if err != nil {
			return // Closed by cleanup.
		}
		resp, ok := d.handleQuery(buf[:n], "udp")
		if !ok {
			continue
		}
		_, _ = d.udp.WriteToUDP(resp, from)
	}
}

// serveTCP accepts TCP connections until the listener is closed.
func (d *dnsServerDouble) serveTCP() {
	for {
		conn, err := d.tcp.Accept()
		if err != nil {
			return
		}
		go d.serveTCPConn(conn)
	}
}

// serveTCPConn reads RFC 1035 section 4.2.2 framed messages until EOF
// and answers each one framed the same way.
func (d *dnsServerDouble) serveTCPConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	for {
		var prefix [2]byte
		if _, err := io.ReadFull(conn, prefix[:]); err != nil {
			return // EOF or closed listener ends the frame loop.
		}
		msg := make([]byte, binary.BigEndian.Uint16(prefix[:]))
		if _, err := io.ReadFull(conn, msg); err != nil {
			return
		}
		resp, ok := d.handleQuery(msg, "tcp")
		if !ok {
			continue
		}
		frame := make([]byte, 2+len(resp))
		binary.BigEndian.PutUint16(frame[:2], uint16(len(resp)))
		copy(frame[2:], resp)
		if _, err := conn.Write(frame); err != nil {
			return
		}
	}
}

// handleQuery unpacks one query, records the name and transport, and
// packs the fixed response.
func (d *dnsServerDouble) handleQuery(wire []byte, transport string) ([]byte, bool) {
	name, query, ok := dnsQueryName(wire)
	if !ok {
		return nil, false
	}
	d.recordQuery(transport, name)
	resp, err := buildDNSResponse(query, d.answerIP)
	if err != nil {
		return nil, false
	}
	return resp, true
}

// dohServerDouble is an RFC 8484 DNS-over-HTTPS endpoint on loopback:
// POST bodies and base64url "dns" GET parameters carry the wire-format
// query; responses are application/dns-message like the DNS double.
type dohServerDouble struct {
	ts       *httptest.Server
	answerIP net.IP

	mu    sync.Mutex
	hits_ int
	asked []string
}

// startDoHServer starts the DoH double and registers its cleanup.
func startDoHServer(t *testing.T, answerIP net.IP) *dohServerDouble {
	t.Helper()
	d := &dohServerDouble{answerIP: answerIP}
	d.ts = httptest.NewServer(http.HandlerFunc(d.handle))
	t.Cleanup(d.ts.Close)
	return d
}

// URL returns the endpoint's http://127.0.0.1:port base URL.
func (d *dohServerDouble) URL() string { return d.ts.URL }

// hits reports how many DNS queries were answered.
func (d *dohServerDouble) hits() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hits_
}

// names returns a snapshot of recorded question names in order.
func (d *dohServerDouble) names() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.asked...)
}

// dohMessageContentType is the RFC 8484 media type.
const dohMessageContentType = "application/dns-message"

// readQuery extracts the wire-format query from an RFC 8484 request,
// writing the RFC status codes for malformed requests.
func (d *dohServerDouble) readQuery(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	switch r.Method {
	case http.MethodPost:
		if ct := r.Header.Get("Content-Type"); ct != dohMessageContentType {
			http.Error(w, "POST requires "+dohMessageContentType, http.StatusUnsupportedMediaType)
			return nil, false
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return nil, false
		}
		return body, true
	case http.MethodGet:
		raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		if err != nil {
			http.Error(w, "bad dns parameter", http.StatusBadRequest)
			return nil, false
		}
		return raw, true
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, false
	}
}

// handle serves one RFC 8484 exchange.
func (d *dohServerDouble) handle(w http.ResponseWriter, r *http.Request) {
	raw, ok := d.readQuery(w, r)
	if !ok {
		return
	}
	name, query, ok := dnsQueryName(raw)
	if !ok {
		http.Error(w, "undecodable DNS message", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.hits_++
	d.asked = append(d.asked, name)
	d.mu.Unlock()
	resp, err := buildDNSResponse(query, d.answerIP)
	if err != nil {
		http.Error(w, "pack response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", dohMessageContentType)
	_, _ = w.Write(resp)
}
