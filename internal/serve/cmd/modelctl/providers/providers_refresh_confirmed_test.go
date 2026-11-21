package providers

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"

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
	_, globalPath, _, err := cmdtest.RunProvidersCmdInIsolatedAppFull(t, providersUpdateCmd, providerFixture(server.URL), "custom", "")
	require.NoError(t, err)

	ids, fresh := confirmedCustom(server.URL)
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
	_, _, _, err := cmdtest.RunProvidersCmdInIsolatedAppFull(t, providersUpdateCmd, providerFixture(server.URL), "custom", "")
	require.NoError(t, err)
	_, fresh := confirmedCustom(server.URL)
	require.False(t, fresh, "cache-only mode never writes the verified roster")
}

func confirmedCustom(endpoint string) ([]string, bool) {
	models, fresh := config.CachedConfirmedModels(
		config.ProviderConfig{Type: "openai-compat", BaseURL: endpoint, APIKey: "test-key"}, "custom", config.IdentityResolver())
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids, fresh
}
