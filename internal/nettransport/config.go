// Package nettransport builds outbound HTTP clients for provider
// connections with an optional proxy (SOCKS5 or HTTP) and an optional
// custom resolver (plain DNS or DNS-over-HTTPS), turning a cascaded
// NetworkConfig into an *http.Client or *http.Transport.
// Four behavior modes exist:
//
//  1. Nothing configured: BuildHTTPClient returns nil so the caller
//     keeps its own default client with byte-identical behavior.
//  2. Proxy only: names resolve THROUGH the proxy. For HTTP proxies
//     http.Transport.Proxy handles it; for SOCKS5 the hostname is
//     passed to the server unresolved (RFC 1928 ATYP=0x03) and the
//     server resolves it.
//  3. Resolver only: names resolve via the configured DNS/DoH server
//     and the connection dials the resolved IP directly over plain
//     TCP. http.Transport does TLS SNI/cert verification from the
//     request's Host, which is why DialContext — not DialTLSContext —
//     is what this package overrides.
//  4. Both: the DNS/DoH lookup itself is tunneled through the proxy
//     and the final destination connection goes through the same
//     proxy.
package nettransport

import (
	"errors"
	"fmt"
	"net"
	"net/url"

	"github.com/PHPCraftdream/rush/internal/config"
)

// ResolveNetworkConfig merges global and per-provider network settings.
// Merging is per FIELD, not per struct: for each of Proxy, DNSServer
// and DoHURL independently, the provider value wins when non-empty,
// otherwise the global value is inherited. A nil argument means
// "nothing configured at that level" and is skipped, never a panic.
func ResolveNetworkConfig(global, provider *config.NetworkConfig) config.NetworkConfig {
	var merged config.NetworkConfig
	for _, src := range []*config.NetworkConfig{global, provider} {
		if src == nil {
			continue
		}
		if src.Proxy != "" {
			merged.Proxy = src.Proxy
		}
		if src.DNSServer != "" {
			merged.DNSServer = src.DNSServer
		}
		if src.DoHURL != "" {
			merged.DoHURL = src.DoHURL
		}
	}
	return merged
}

// resolved is the normalized, validated view of a NetworkConfig used
// internally by the package builders.
type resolved struct {
	proxyURL  *url.URL
	dnsServer string
	dohURL    string
}

// empty reports whether nothing at all is configured; builders use it
// to keep the zero-config path identical to having no override.
func (r resolved) empty() bool {
	return r.proxyURL == nil && r.dnsServer == "" && r.dohURL == ""
}

// resolveConfig normalizes and validates a NetworkConfig. DoHURL wins
// over DNSServer entirely when both are set (documented precedence,
// not an error): dns_server is ignored. A DNSServer without a port
// gets the default DNS port 53 appended.
func resolveConfig(cfg config.NetworkConfig) (resolved, error) {
	var rs resolved

	if cfg.Proxy != "" {
		pu, err := url.Parse(cfg.Proxy)
		if err != nil {
			return resolved{}, fmt.Errorf("parse proxy URL %s: %w",
				redactedURL(cfg.Proxy), urlErrReason(err))
		}
		switch pu.Scheme {
		case "socks5", "socks5h":
			// socks5h is an alias of socks5 here: this package always
			// lets the proxy resolve names in proxy-only mode (RFC 1928
			// ATYP=0x03), which is exactly what socks5h explicitly
			// requests.
		case "http":
			// Handled via a CONNECT tunnel.
		default:
			return resolved{}, fmt.Errorf(
				"unsupported proxy scheme %q in %s (supported: socks5, socks5h, http)",
				pu.Scheme, redactedURL(cfg.Proxy))
		}
		if pu.Host == "" {
			return resolved{}, fmt.Errorf(
				"proxy URL %s has no host", redactedURL(cfg.Proxy))
		}
		rs.proxyURL = pu
	}

	switch {
	case cfg.DoHURL != "":
		if _, err := url.Parse(cfg.DoHURL); err != nil {
			return resolved{}, fmt.Errorf("parse DoH URL %s: %w",
				redactedURL(cfg.DoHURL), urlErrReason(err))
		}
		rs.dohURL = cfg.DoHURL
	case cfg.DNSServer != "":
		server := cfg.DNSServer
		if _, _, err := net.SplitHostPort(server); err != nil {
			// No port given: normalize so dialers always see host:port.
			server = net.JoinHostPort(server, "53")
		}
		rs.dnsServer = server
	}

	return rs, nil
}

// redactedURL renders a raw URL string safe for error messages:
// userinfo, query and fragment are stripped so secrets in those
// components cannot leak into persisted diagnostics, while scheme,
// host and path stay for troubleshootability. Input that does not
// parse degrades to a fixed placeholder instead of echoing
// potentially secret-bearing text.
func redactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL)"
	}
	return redactedURLValue(u)
}

// redactedURLValue is redactedURL for an already-parsed URL.
func redactedURLValue(u *url.URL) string {
	clone := *u
	clone.User = nil
	clone.RawQuery = ""
	clone.ForceQuery = false
	clone.Fragment = ""
	clone.RawFragment = ""
	return clone.String()
}

// urlErrReason unwraps a *url.Error to its underlying reason: the
// stdlib error string embeds the raw input URL via its Op and URL
// fields, which would defeat the redaction of the surrounding
// message.
func urlErrReason(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
