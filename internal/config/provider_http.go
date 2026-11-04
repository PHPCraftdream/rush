package config

import (
	"net/http"
	"sync/atomic"
	"time"
)

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
