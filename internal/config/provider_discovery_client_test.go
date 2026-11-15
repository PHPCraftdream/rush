package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/PHPCraftdream/rush/internal/kit/sys/env"
	"github.com/stretchr/testify/require"
)

// routingTransport forwards every request to the test server, whatever host it
// names, and counts them: a stand-in for a proxied provider transport.
type routingTransport struct {
	target *url.URL
	hits   atomic.Int32
}

func (rt *routingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.hits.Add(1)
	forwarded := r.Clone(r.Context())
	forwarded.URL.Scheme, forwarded.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(forwarded)
}

func providerRosterServer(t *testing.T, body string) (*routingTransport, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	return &routingTransport{target: target}, server.Close
}

// recordFactoryArgs installs a factory that hands every provider the given
// transport and records the network blocks it was asked to resolve.
func recordFactoryArgs(t *testing.T, transport http.RoundTripper) *[]*NetworkConfig {
	t.Helper()
	var providers []*NetworkConfig
	t.Cleanup(SetProviderHTTPClientFactory(func(_, provider *NetworkConfig) (*http.Client, error) {
		providers = append(providers, provider)
		return &http.Client{Transport: transport}, nil
	}))
	return &providers
}

func configureOne(t *testing.T, cfg *Config, known []catwalk.Provider, vars map[string]string) {
	t.Helper()
	setDefaults(cfg, t.TempDir(), "")
	environ := env.NewFromMap(vars)
	require.NoError(t, configureProviders(context.Background(), cfg, environ, NewShellVariableResolver(environ), known, testStore(cfg).removeConfigFieldBestEffort, nil))
}

func TestCustomProviderAutoDiscoveryUsesTheProviderClient(t *testing.T) {
	isolateConfigEnvironment(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "")
	transport, closeServer := providerRosterServer(t, `{"data":[{"id":"discovered-model"}]}`)
	defer closeServer()
	seen := recordFactoryArgs(t, transport)

	providerNetwork := &NetworkConfig{Proxy: "none"}
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.Providers.Set("custom", ProviderConfig{
		ID: "custom", Type: "openai-compat", BaseURL: "http://models.invalid/v1", APIKey: "k",
		Network: providerNetwork,
	})
	configureOne(t, cfg, nil, nil)

	provider, ok := cfg.Providers.Get("custom")
	require.True(t, ok)
	_, found := findCatalogModel(provider.Models, "discovered-model")
	require.True(t, found, "auto-discovery must reach the provider through its own client")
	require.Positive(t, transport.hits.Load())
	require.Contains(t, *seen, providerNetwork, "the provider's own network block drives its discovery client")
}

func TestKnownProviderCatalogDiscoveryUsesTheProviderClient(t *testing.T) {
	isolateConfigEnvironment(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "")
	transport, closeServer := providerRosterServer(t, `{"data":[{"id":"live-step-model"}]}`)
	defer closeServer()
	seen := recordFactoryArgs(t, transport)

	providerNetwork := &NetworkConfig{Proxy: "socks5://127.0.0.1:1080"}
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.Providers.Set("stepfun", ProviderConfig{APIKey: "$STEPFUN_API_KEY", Network: providerNetwork})
	known := []catwalk.Provider{{
		ID:          "stepfun",
		APIKey:      "$STEPFUN_API_KEY",
		APIEndpoint: "http://stepfun.invalid/v1",
		Models:      []catwalk.Model{{ID: "configured-model"}},
	}}
	configureOne(t, cfg, known, map[string]string{"STEPFUN_API_KEY": "step-key"})

	provider, ok := cfg.Providers.Get("stepfun")
	require.True(t, ok)
	_, found := findCatalogModel(provider.Models, "live-step-model")
	require.True(t, found, "the live catalog must be fetched through the provider's client")
	require.Positive(t, transport.hits.Load())
	require.Contains(t, *seen, providerNetwork)
}

func TestProviderDiscoveryWithMalformedNetworkSettingsDoesNotGoDirect(t *testing.T) {
	isolateConfigEnvironment(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "")
	var direct atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		direct.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"must-not-appear"}]}`))
	}))
	defer server.Close()
	t.Cleanup(SetProviderHTTPClientFactory(func(_, _ *NetworkConfig) (*http.Client, error) {
		return nil, context.DeadlineExceeded // any construction failure
	}))

	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.Providers.Set("custom", ProviderConfig{
		ID: "custom", Type: "openai-compat", BaseURL: server.URL + "/v1", APIKey: "k",
	})
	configureOne(t, cfg, nil, nil)

	// A model-less provider whose discovery fails is dropped by configureProviders
	// (existing behavior); either way nothing may have been fetched directly.
	if provider, ok := cfg.Providers.Get("custom"); ok {
		_, found := findCatalogModel(provider.Models, "must-not-appear")
		require.False(t, found, "a provider whose network settings cannot be built must not fall back to a direct connection")
	}
	require.Zero(t, direct.Load(), "no direct request may be made")
}

func TestProviderModelVerificationUsesTheProviderClient(t *testing.T) {
	isolateConfigEnvironment(t)
	t.Cleanup(SetModelVerifyOnlineForTests(true))
	for _, tc := range []struct {
		name, providerType, base, body string
	}{
		{"anthropic", "anthropic", "http://anthropic.invalid", `{"data":[{"id":"claude-live"}],"has_more":false}`},
		{"openai-compat", "openai-compat", "http://models.invalid/v1", `{"data":[{"id":"claude-live"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport, closeServer := providerRosterServer(t, tc.body)
			defer closeServer()
			seen := recordFactoryArgs(t, transport)
			providerNetwork := &NetworkConfig{Proxy: "http://provider-proxy:8080"}
			cfg := &Config{Options: &Options{Network: &NetworkConfig{Proxy: "socks5://global:1080"}}}
			pc := ProviderConfig{Type: catwalk.Type(tc.providerType), BaseURL: tc.base, APIKey: "k", Network: providerNetwork}

			models, reason, err := FetchProviderModels(context.Background(), cfg, pc, "p", IdentityResolver())
			require.NoError(t, err)
			require.Empty(t, reason)
			_, found := findCatalogModel(models, "claude-live")
			require.True(t, found)
			require.Positive(t, transport.hits.Load(), "the verification request must travel through the provider client")
			require.Contains(t, *seen, providerNetwork)
		})
	}
}

func TestProviderModelVerificationReportsInvalidNetworkSettings(t *testing.T) {
	isolateConfigEnvironment(t)
	t.Cleanup(SetModelVerifyOnlineForTests(true))
	var direct atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		direct.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"x"}]}`))
	}))
	defer server.Close()
	t.Cleanup(SetProviderHTTPClientFactory(func(_, _ *NetworkConfig) (*http.Client, error) {
		return nil, context.Canceled
	}))
	pc := ProviderConfig{Type: "openai-compat", BaseURL: server.URL + "/v1", APIKey: "k"}
	models, reason, err := FetchProviderModels(context.Background(), &Config{}, pc, "p", IdentityResolver())
	require.NoError(t, err)
	require.Nil(t, models)
	require.Contains(t, reason, "provider network settings are invalid")
	require.Zero(t, direct.Load(), "an invalid setting must not fall back to a direct request")
}

func TestConfigureProvidersWithScope_DiscoveryScoping(t *testing.T) {
	type testCase struct {
		name          string
		scope         map[string]bool
		codexScoped   bool
		wantCodexHits int32
		wantZAIHits   int32
		wantDocsCalls int32
	}
	cases := []testCase{
		{name: "scoped to zai", scope: map[string]bool{"zai": true}, codexScoped: false, wantCodexHits: 0, wantZAIHits: 1, wantDocsCalls: 1},
		{name: "no scope", scope: nil, wantCodexHits: 1, wantZAIHits: 1, wantDocsCalls: 1},
		{name: "scoped to codex", scope: map[string]bool{"openai-codex": true}, wantCodexHits: 1, wantZAIHits: 0, wantDocsCalls: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigEnvironment(t)
			t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "")
			uniqueSuffix := tc.name

			var codexHits, zaiHits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("chatgpt-account-id") != "" {
					codexHits.Add(1)
				} else {
					zaiHits.Add(1)
				}
				_, _ = w.Write([]byte(`{"data":[{"id":"discovered-model"}]}`))
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			recordFactoryArgs(t, &routingTransport{target: target})

			var docsCalls atomic.Int32
			originalDocs := discoverZAIEffortDocs
			discoverZAIEffortDocs = func(context.Context) ([]catwalk.Model, error) {
				docsCalls.Add(1)
				return []catwalk.Model{{ID: "glm-5", ReasoningLevels: []string{"low", "high"}}}, nil
			}
			t.Cleanup(func() { discoverZAIEffortDocs = originalDocs })

			cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
			setDefaults(cfg, t.TempDir(), "")
			cfg.Providers.Set(string(catwalk.InferenceProviderZAI), ProviderConfig{
				ID:      string(catwalk.InferenceProviderZAI),
				BaseURL: server.URL,
				APIKey:  "zai-key-" + uniqueSuffix,
			})
			cfg.Providers.Set("openai-codex", ProviderConfig{
				ID:         "openai-codex",
				OAuthToken: &oauth.Token{AccessToken: "tok-" + uniqueSuffix, AccountID: "acc-" + uniqueSuffix},
			})
			known := []catwalk.Provider{zaiKnownProvider(), codexProvider()}
			environ := env.NewFromMap(nil)
			resolver := NewShellVariableResolver(environ)

			require.NoError(t, configureProvidersWithScope(context.Background(), cfg, environ, resolver, known, testStore(cfg).removeConfigFieldBestEffort, nil, tc.scope))

			require.GreaterOrEqual(t, codexHits.Load(), tc.wantCodexHits)
			require.GreaterOrEqual(t, zaiHits.Load(), tc.wantZAIHits)
			require.GreaterOrEqual(t, docsCalls.Load(), tc.wantDocsCalls)
			if tc.wantCodexHits == 0 {
				require.Equal(t, int32(0), codexHits.Load(), "scoped-out provider must receive zero discovery requests")
			}
			if tc.wantZAIHits == 0 {
				require.Equal(t, int32(0), zaiHits.Load(), "scoped-out provider must receive zero discovery requests")
			}
			if tc.wantDocsCalls == 0 {
				require.Equal(t, int32(0), docsCalls.Load())
			}

			pc, ok := cfg.Providers.Get(string(catwalk.InferenceProviderZAI))
			require.True(t, ok, "zai provider must stay registered")
			if tc.wantDocsCalls > 0 {
				_, found := findCatalogModel(pc.Models, "discovered-model")
				require.True(t, found, "zai provider must merge the discovered model")
			}
			codex, ok := cfg.Providers.Get("openai-codex")
			require.True(t, ok, "codex provider must stay registered even when its discovery is skipped")
			if tc.wantCodexHits == 0 {
				require.Empty(t, codex.Models, "skipped codex discovery must leave its models empty")
			}
		})
	}
}
