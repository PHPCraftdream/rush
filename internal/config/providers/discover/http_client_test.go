package discover

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

// routeToServer is a stand-in for a provider's proxied transport: it counts
// requests and forwards them to the test server, whatever host they name.
type routeToServer struct {
	target *url.URL
	hits   atomic.Int32
}

func (rt *routeToServer) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.hits.Add(1)
	forwarded := r.Clone(r.Context())
	forwarded.URL.Scheme = rt.target.Scheme
	forwarded.URL.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(forwarded)
}

func routedClient(t *testing.T, handler http.HandlerFunc) (*http.Client, *routeToServer) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	transport := &routeToServer{target: target}
	return &http.Client{Transport: transport}, transport
}

const unreachableBase = "http://models.invalid/v1"

// The provider's own client must carry every discovery request; the shared
// default client could never resolve models.invalid, so a request that
// bypassed the provider client would fail.
func TestDiscoveryUsesTheProviderClient(t *testing.T) {
	roster := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
	}

	t.Run("DiscoverModels", func(t *testing.T) {
		client, transport := routedClient(t, roster)
		models, err := DiscoverModels(context.Background(), Config{ID: "p", BaseURL: unreachableBase, APIKey: "k", HTTPClient: client}, &mockResolver{})
		require.NoError(t, err)
		require.Len(t, models, 2)
		require.EqualValues(t, 1, transport.hits.Load())
	})

	t.Run("DiscoverProviderCatalog", func(t *testing.T) {
		client, transport := routedClient(t, roster)
		models, err := DiscoverProviderCatalog(context.Background(), Config{ID: "p", BaseURL: unreachableBase, APIKey: "k", HTTPClient: client}, &mockResolver{})
		require.NoError(t, err)
		require.Len(t, models, 2)
		require.EqualValues(t, 1, transport.hits.Load())
	})

	t.Run("DiscoverAnthropicModels", func(t *testing.T) {
		client, transport := routedClient(t, func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "secret", r.Header.Get("x-api-key"))
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-x"}],"has_more":false}`))
		})
		models, err := DiscoverAnthropicModels(context.Background(), client, "http://anthropic.invalid", "secret")
		require.NoError(t, err)
		require.Len(t, models, 1)
		require.EqualValues(t, 1, transport.hits.Load())
	})

	t.Run("enricher requests use it too", func(t *testing.T) {
		client, transport := routedClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"model_info":{"llama.context_length":4096},"capabilities":[]}`))
		})
		enricher := GetEnricher("ollama")
		require.NotNil(t, enricher)
		cfg := Config{ID: "p", BaseURL: "http://ollama.invalid/v1", HTTPClient: client}
		_, err := enricher.EnrichModels(context.Background(), cfg, &mockResolver{}, []catwalk.Model{{ID: "llama3:latest"}})
		require.NoError(t, err)
		require.Positive(t, transport.hits.Load(), "the ollama /api/show enrichment must go through the provider client")
	})
}

func TestDiscoveryWithoutProviderClientUsesTheDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"only"}]}`))
	}))
	defer server.Close()
	models, err := DiscoverModels(context.Background(), Config{ID: "p", BaseURL: server.URL + "/v1"}, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 1)
}
