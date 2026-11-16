package cfgmodel

import (
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

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

// ProviderHTTPClientFactory builds the HTTP client that carries a provider's
// outbound network policy (proxy, custom DNS/DoH) from the global
// options.network block and the provider's own network block. It returns a
// nil client when nothing is configured. The implementation lives in
// internal/nettransport, which imports this package, so it registers itself
// here from its init (the database/sql driver pattern); a binary that does
// not link nettransport sends provider-bound requests directly.
type ProviderHTTPClientFactory func(global, provider *NetworkConfig) (*http.Client, error)

var providerHTTPClientFactory atomic.Pointer[ProviderHTTPClientFactory]

// SetProviderHTTPClientFactory installs the factory and returns a function
// restoring the previous one.
func SetProviderHTTPClientFactory(factory ProviderHTTPClientFactory) func() {
	previous := providerHTTPClientFactory.Swap(&factory)
	return func() { providerHTTPClientFactory.Store(previous) }
}

// providerRequestTimeout bounds a single provider-bound request that is not
// an LLM call (model-list discovery and verification).
const providerRequestTimeout = 10 * time.Second

// ProviderHTTPClient returns the client for provider-bound requests that are
// not LLM calls, following the same global -> provider cascade (and the
// `proxy: none` opt-out) as the LLM client. A nil client with a nil error
// means "nothing configured, use the default client". A non-nil error is a
// malformed network setting: callers must fail the request rather than fall
// back to a direct connection. A nil Config means "no global settings".
func (c *Config) ProviderHTTPClient(pc ProviderConfig) (*http.Client, error) {
	factory := providerHTTPClientFactory.Load()
	if factory == nil || *factory == nil {
		return nil, nil
	}
	var global *NetworkConfig
	if c != nil && c.Options != nil {
		global = c.Options.Network
	}
	client, err := (*factory)(global, pc.Network)
	if err != nil || client == nil {
		return nil, err
	}
	return &http.Client{
		Transport:     client.Transport,
		CheckRedirect: client.CheckRedirect,
		Jar:           client.Jar,
		Timeout:       providerRequestTimeout,
	}, nil
}
