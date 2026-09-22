package nettransport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

// transportTLSHandshakeTimeout and transportIdleConnTimeout mirror
// http.DefaultTransport's values. Zero TLSHandshakeTimeout lets a
// stuck TLS handshake hang a provider connection forever; zero
// IdleConnTimeout keeps keep-alive sockets open indefinitely after
// their last use.
const (
	transportTLSHandshakeTimeout = 10 * time.Second
	transportIdleConnTimeout     = 90 * time.Second

	// transportDialTimeout bounds one initial TCP connect attempt
	// made by transport-managed dialers. Without it stdlib's
	// zero-value dialer (no Timeout of its own) governs the connect:
	// the TLS/CONNECT/idle budgets only start AFTER the TCP
	// handshake, and net/http detaches the dial context from request
	// cancellation, so neither the request's cancel nor a client
	// timeout ends a stuck dial.
	transportDialTimeout = 30 * time.Second

	// resolvedDialBudget bounds the WHOLE fallback sequence across a
	// resolved address list (all A records, then AAAA), rather than
	// each attempt separately.
	resolvedDialBudget = 2 * transportDialTimeout
)

// transportDialer is the shared bounded dialer for
// transport-managed initial TCP connects; net.Dialer is safe for
// concurrent use.
var transportDialer = &net.Dialer{Timeout: transportDialTimeout}

// BuildHTTPClient returns a *http.Client configured per cfg's proxy/DNS
// settings, or nil if cfg is the zero value (nothing configured — the
// caller keeps its own default client unchanged, byte-identical to the
// no-override behavior).
func BuildHTTPClient(cfg config.NetworkConfig) (*http.Client, error) {
	tr, err := BuildTransport(cfg)
	if err != nil {
		return nil, err
	}
	if tr == nil {
		return nil, nil
	}
	return &http.Client{Transport: tr}, nil
}

// BuildTransport returns an *http.Transport per cfg's proxy/DNS
// settings, or nil when nothing is configured at all — no transport is
// built in that case so the caller can keep its own default.
//
// Transports are cached per distinct resolved NetworkConfig and shared
// by every caller with the same configuration, mirroring
// http.DefaultTransport's process-wide sharing: every fresh transport
// owns keep-alive pools (and, for DoH, a second transport hidden in
// the resolver closure), so building one per provider build or model
// override would let idle connections grow with the number of builds
// instead of active calls. The cache is small and LRU-bounded
// (transportCacheCapacity); evicting an entry closes its idle
// connections immediately, and every transport also carries
// transportIdleConnTimeout as a backstop.
func BuildTransport(cfg config.NetworkConfig) (*http.Transport, error) {
	return buildTransportWithCache(cfg, sharedTransports)
}

// buildTransportWithCache is BuildTransport parameterized over the
// transport cache, so tests can drive the exact production assembly
// against an isolated cache instead of the process-wide shared one
// (audit R2-6).
func buildTransportWithCache(cfg config.NetworkConfig, cache *transportCache) (*http.Transport, error) {
	rs, err := resolveConfig(cfg)
	if err != nil {
		return nil, err
	}
	if rs.empty() {
		return nil, nil
	}
	if tr := cache.sharedTransport(rs); tr != nil {
		return tr, nil
	}

	var proxyDial dialFunc
	if rs.proxyURL != nil {
		proxyDial, err = proxyDialer(rs.proxyURL)
		if err != nil {
			return nil, err
		}
	}

	resolver, resolverTransports, err := buildResolver(rs, proxyDial)
	if err != nil {
		return nil, err
	}

	// ForceAttemptHTTP2 mirrors http.DefaultTransport's automatic h2
	// upgrade behavior for a transport built from scratch; the two
	// timeouts mirror its defaults (see the constants above).
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: transportTLSHandshakeTimeout,
		IdleConnTimeout:     transportIdleConnTimeout,
	}

	switch {
	case resolver != nil:
		// Custom resolution is active, so Proxy stays NIL: we tunnel to
		// the resolved address ourselves via DialContext. Setting
		// tr.Proxy as well would make an HTTP proxy CONNECT to the raw
		// hostname and re-resolve it proxy-side, defeating the custom
		// resolver; instead the final dial CONNECTs to the resolved
		// IP:port.
		tr.DialContext = resolvedDialer(resolver, proxyDial)
	case rs.proxyURL.Scheme == "http":
		// Proxy-only HTTP: the stdlib handles CONNECT/absolute-form
		// requests and proxy-side name resolution for free. With Proxy
		// set, stdlib dials the PROXY address through DialContext; the
		// bound matters because stdlib's zero dialer has no timeout of
		// its own and the dial context is detached from request
		// cancellation (R6-1).
		tr.Proxy = http.ProxyURL(rs.proxyURL)
		tr.DialContext = transportDialer.DialContext
	default:
		// Proxy-only SOCKS5: the SOCKS5 dialer IS the tunnel, and
		// hostnames reach the server unresolved for proxy-side
		// resolution (RFC 1928 ATYP=0x03).
		tr.DialContext = proxyDial
	}

	cache.cacheTransport(rs, tr, resolverTransports)
	return tr, nil
}

// resolvedDialer dials "host:port" addresses, resolving non-IP
// hostnames through the custom resolver first, then falls back
// through the full resolved address list (A records then AAAA) under
// one overall budget (resolvedDialBudget). TLS SNI and certificate
// verification are untouched: http.Transport layers TLS on the
// returned connection using the request's original hostname. When
// proxyDial is non-nil the TARGET connection also goes through the
// proxy; dialing the already-resolved IP is deliberate — it avoids a
// second, redundant name resolution inside the SOCKS5 server, since
// with a custom resolver configured the client-side answer is
// authoritative.
func resolvedDialer(resolver resolveFunc, proxyDial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("split address %q: %w", addr, err)
		}
		targets := []string{addr}
		if net.ParseIP(host) == nil {
			ips, err := resolver(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("resolver returned no addresses for %q", host)
			}
			targets = make([]string, 0, len(ips))
			for _, ip := range ips {
				targets = append(targets, net.JoinHostPort(ip.String(), port))
			}
		}
		dialCtx, cancel := context.WithTimeout(ctx, resolvedDialBudget)
		defer cancel()
		errs := make([]error, 0, len(targets))
		for i, target := range targets {
			// The budget is only consulted from the second attempt
			// on, so the first attempt always runs.
			if i > 0 && dialCtx.Err() != nil {
				break
			}
			conn, err := dialTarget(dialCtx, network, target, proxyDial)
			if err == nil {
				return conn, nil
			}
			errs = append(errs, err)
		}
		return nil, fmt.Errorf("dial %q: %w", addr, errors.Join(errs...))
	}
}

// dialTarget dials one resolved address through proxyDial when
// set, or a bounded direct TCP dial otherwise.
func dialTarget(ctx context.Context, network, target string,
	proxyDial dialFunc,
) (net.Conn, error) {
	if proxyDial != nil {
		return proxyDial(ctx, network, target)
	}
	d := net.Dialer{Timeout: transportDialTimeout}
	return d.DialContext(ctx, network, target)
}
