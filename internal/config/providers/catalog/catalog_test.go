package catalog

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestCatalogConstants(t *testing.T) {
	t.Parallel()
	require.Equal(t, "hyper", HyperName)
	require.Equal(t, "local-cli", LocalCLIProviderID)
	require.Equal(t, "cli", LocalCLIProviderType)
	require.Equal(t, "https://hyper.charm.land", hyperDefaultBaseURL)
}

func TestHyperBaseURLFrom(t *testing.T) {
	t.Parallel()
	require.Equal(t, "https://hyper.charm.land", hyperBaseURLFrom(""))
	require.Equal(t, "http://example:9000", hyperBaseURLFrom("http://example:9000"))
}

func TestHyperEmbeddedFromBytes(t *testing.T) {
	t.Parallel()

	valid := []byte(`{"id":"hyper","name":"Charm Hyper","models":[{"id":"m1","name":"M1"}]}`)

	p := hyperEmbeddedFromBytes(valid, "")
	require.Equal(t, catwalk.InferenceProvider("hyper"), p.ID)
	require.Empty(t, p.APIEndpoint)

	p = hyperEmbeddedFromBytes(valid, "http://example:9000")
	require.Equal(t, "http://example:9000/api/v1/fantasy", p.APIEndpoint)

	p = hyperEmbeddedFromBytes([]byte("{not-json"), "")
	require.Equal(t, catwalk.Provider{}, p)
}

func TestEmbeddedProviderJSONCatalog(t *testing.T) {
	p := HyperEmbedded()
	require.Equal(t, catwalk.InferenceProvider("hyper"), p.ID)
	require.Greater(t, len(p.Models), 0)
}
