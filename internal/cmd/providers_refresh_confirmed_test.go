package cmd

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func providerFixture(endpoint string) string {
	return `{"providers":{"custom":{"name":"Custom","type":"openai-compat","base_url":"` + endpoint + `","api_key":"test-key","models":[{"id":"seed-model"}]}}}`
}

func TestProvidersUpdateCachesConfirmedRoster(t *testing.T) {
	t.Cleanup(config.SetModelVerifyOnlineForTests(true))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"future-model","context_window":64000}]}`))
	}))
	defer server.Close()
	_, globalPath, _, err := runProvidersCmdInIsolatedAppFull(t, providersUpdateCmd, providerFixture(server.URL), "custom", "")
	require.NoError(t, err)

	ids, fresh := confirmedCustom(globalPath, server.URL)
	require.True(t, fresh)
	require.Equal(t, []string{"future-model"}, ids)

	config.ResetProviderCacheForTests()
	loaded, err := config.Load("", filepath.Dir(globalPath), false)
	require.NoError(t, err)
	provider, ok := loaded.Config().Providers.Get("custom")
	require.True(t, ok)
	found := false
	for _, model := range provider.Models {
		if model.ID == "future-model" {
			found = true
		}
	}
	require.True(t, found, "a fresh load includes the cached provider model")
}

func TestProvidersUpdateCacheOnlyDoesNotCacheRoster(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"future-model"}]}`))
	}))
	defer server.Close()
	_, globalPath, _, err := runProvidersCmdInIsolatedAppFull(t, providersUpdateCmd, providerFixture(server.URL), "custom", "")
	require.NoError(t, err)
	_, fresh := confirmedCustom(globalPath, server.URL)
	require.False(t, fresh, "cache-only mode never writes the verified roster")
}
