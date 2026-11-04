package agent

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// fetchViaProviderClient sends one GET through the provider's resolved
// client (a nil client means "direct": the SDK default) and returns the
// body.
func fetchViaProviderClient(t *testing.T, client *http.Client, target string) string {
	t.Helper()
	if client == nil {
		client = &http.Client{}
	} else {
		t.Cleanup(client.CloseIdleConnections)
	}
	client.Timeout = 10 * time.Second
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

// TestProviderProxyCascade proves the global -> provider proxy cascade
// end to end: which proxy double actually sees the request.
func TestProviderProxyCascade(t *testing.T) {
	t.Parallel()

	type step struct {
		name         string
		providerNet  func(globalProxy, providerProxy string) *config.NetworkConfig
		wantGlobal   bool
		wantProvider bool
	}
	steps := []step{
		{
			name:        "provider without network uses the global proxy",
			providerNet: func(string, string) *config.NetworkConfig { return nil },
			wantGlobal:  true,
		},
		{
			name:        "provider with empty proxy inherits the global proxy",
			providerNet: func(string, string) *config.NetworkConfig { return &config.NetworkConfig{} },
			wantGlobal:  true,
		},
		{
			name: "provider proxy replaces the global proxy",
			providerNet: func(_, providerProxy string) *config.NetworkConfig {
				return &config.NetworkConfig{Proxy: providerProxy}
			},
			wantProvider: true,
		},
		{
			name: "provider none bypasses the global proxy",
			providerNet: func(string, string) *config.NetworkConfig {
				return &config.NetworkConfig{Proxy: config.ProxyNone}
			},
		},
		{
			name: "provider NONE is case insensitive",
			providerNet: func(string, string) *config.NetworkConfig {
				return &config.NetworkConfig{Proxy: "NONE"}
			},
		},
	}

	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := startTargetDouble(t, http.StatusOK, "text/plain", probeTargetBody)
			authority := hostPortOfURL(t, target.ts.URL)
			global := startProxyDouble(t, authority)
			provider := startProxyDouble(t, authority)

			coord := &coordinator{cfg: newNetworkTestStore(t, &config.Options{
				Network: &config.NetworkConfig{Proxy: global.URL()},
			})}
			client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{
				ID:      "probe",
				Network: tc.providerNet(global.URL(), provider.URL()),
			})
			require.NoError(t, err)

			require.Equal(t, probeTargetBody, fetchViaProviderClient(t, client, target.ts.URL))
			require.GreaterOrEqual(t, target.hitCount(), 1)
			require.Equal(t, tc.wantGlobal, len(global.seen()) > 0, "global proxy seen=%v", global.seen())
			require.Equal(t, tc.wantProvider, len(provider.seen()) > 0, "provider proxy seen=%v", provider.seen())
		})
	}
}

// TestProviderProxyNoneWithoutGlobalProxyIsDirect covers the "no global
// proxy, provider says none" corner: nothing is configured, so the
// resolved client stays nil (the SDK default).
func TestProviderProxyNoneWithoutGlobalProxyIsDirect(t *testing.T) {
	t.Parallel()
	coord := &coordinator{cfg: newNetworkTestStore(t, nil)}
	client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{
		ID:      "probe",
		Network: &config.NetworkConfig{Proxy: config.ProxyNone},
	})
	require.NoError(t, err)
	require.Nil(t, client)
}

// TestProviderProxyNoneKeepsGlobalResolver proves "none" clears only the
// proxy: a global dns_server still applies, in resolver-only (direct
// dial) mode.
func TestProviderProxyNoneKeepsGlobalResolver(t *testing.T) {
	t.Parallel()
	coord := &coordinator{cfg: newNetworkTestStore(t, &config.Options{
		Network: &config.NetworkConfig{Proxy: "http://127.0.0.1:1", DNSServer: "1.1.1.1"},
	})}
	client, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{
		ID:      "probe",
		Network: &config.NetworkConfig{Proxy: config.ProxyNone},
	})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(client.CloseIdleConnections)
	tr, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, tr.DialContext, "resolver stays active")
	require.Nil(t, tr.Proxy, "proxy is gone")
}

// TestProviderProxyNoneIsNotMalformed guards the validation path: "none"
// must never reach the URL parser as an unsupported scheme.
func TestProviderProxyNoneIsNotMalformed(t *testing.T) {
	t.Parallel()
	for _, global := range []string{"", "http://127.0.0.1:1", "socks5://127.0.0.1:1"} {
		opts := &config.Options{}
		if global != "" {
			opts.Network = &config.NetworkConfig{Proxy: global}
		}
		coord := &coordinator{cfg: newNetworkTestStore(t, opts)}
		_, err := coord.resolveProviderHTTPClient(coord.cfg.Config(), config.ProviderConfig{
			ID:      "probe",
			Network: &config.NetworkConfig{Proxy: "none"},
		})
		require.NoError(t, err, "global=%q", global)
	}
}
