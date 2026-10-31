package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestLiteralCredentialBuilderRejectsGoogleVertex(t *testing.T) {
	coord := &coordinator{}
	provider, err := coord.buildProviderWithLiteralCredentials(nil, config.ProviderConfig{
		ID: "tenant", Type: "google-vertex", APIKey: "tenant-key",
	}, config.SelectedModel{}, false)
	require.ErrorContains(t, err, "google-vertex credentials are unsupported")
	require.Nil(t, provider)
}

func TestLiteralCredentialBuilderIgnoresCodexIDAndOAuth(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	store := config.NewLibraryStore(&config.Config{Options: &config.Options{}}, t.TempDir())
	store.SetProviderRuntimeConfig("openai-codex", config.ProviderConfig{
		ID: "openai-codex", Type: "openai-compat",
		OAuthToken: &oauth.Token{AccessToken: "operator-oauth-token"},
	})
	coord := &coordinator{cfg: store}
	providerCfg := config.ProviderConfig{
		ID: "openai-codex", Type: "openai", APIKey: "tenant-api-key", BaseURL: server.URL,
		OAuthToken: &oauth.Token{AccessToken: "provider-oauth-token"},
	}
	provider, err := coord.buildProviderWithLiteralCredentials(store.Config(), providerCfg, config.SelectedModel{}, false)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "tenant-model")
	require.NoError(t, err)
	_, _ = model.Generate(t.Context(), fantasy.Call{})
	require.Equal(t, "Bearer tenant-api-key", gotAuth)
}
