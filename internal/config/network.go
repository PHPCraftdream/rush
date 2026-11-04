package config

import "strings"

// ProxyNone is the proxy value that switches the proxy off at its level.
// On a provider it cancels the global proxy; globally it means "no proxy".
const ProxyNone = "none"

// IsProxyNone reports whether proxy is the explicit "no proxy" value.
func IsProxyNone(proxy string) bool {
	return strings.EqualFold(strings.TrimSpace(proxy), ProxyNone)
}

// NetworkConfig configures outbound network behavior for provider HTTP
// connections: an optional proxy and an optional custom DNS/DoH resolver.
// A zero value means "use OS defaults", identical to today's behavior.
type NetworkConfig struct {
	// Proxy is a proxy URL for outbound connections, e.g.
	// "socks5://127.0.0.1:1080" or "http://127.0.0.1:8080" (userinfo for
	// auth is supported: "socks5://user:pass@host:port"). A provider's
	// proxy replaces the global one entirely; "none" on a provider
	// disables the proxy for it even when a global proxy is set.
	Proxy string `json:"proxy,omitempty" jsonschema:"description=Proxy URL (socks5:// or http://) for outbound provider connections. A provider's proxy replaces the global one; 'none' on a provider disables the proxy for it even when a global proxy is set,example=socks5://127.0.0.1:1080"`
	// DNSServer is a plain DNS server address (host, or host:port —
	// port defaults to 53) used to resolve provider hostnames instead
	// of the OS resolver.
	DNSServer string `json:"dns_server,omitempty" jsonschema:"description=Custom DNS server (host or host:port\\, default port 53) for resolving provider hostnames,example=1.1.1.1"`
	// DoHURL is a DNS-over-HTTPS endpoint used to resolve provider
	// hostnames instead of the OS resolver. If both DNSServer and DoHURL
	// are set, DoHURL wins (documented precedence, not a config error).
	DoHURL string `json:"doh_url,omitempty" jsonschema:"description=DNS-over-HTTPS endpoint for resolving provider hostnames; wins over dns_server if both are set,example=https://cloudflare-dns.com/dns-query"`
}
