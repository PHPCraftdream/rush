package config

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/PHPCraftdream/rush/internal/kit/sys/env"
	"github.com/stretchr/testify/require"
)

func TestConfigureProviders_CodexOAuthWithUnavailableCatalogKeepsConfigLoadable(t *testing.T) {
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	setDefaults(cfg, "/tmp", "")
	cfg.Options.DisableDefaultProviders = true
	cfg.Providers.Set("openai-codex", ProviderConfig{
		ID:         "openai-codex",
		OAuthToken: &oauth.Token{AccessToken: "rush-access-token", AccountID: "chatgpt-account"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resolver := NewShellVariableResolver(env.NewFromMap(nil))
	err := configureProviders(ctx, cfg, env.NewFromMap(nil), resolver, nil, testStore(cfg).RemoveConfigFieldBestEffort, nil)
	require.NoError(t, err)

	provider, ok := cfg.Providers.Get("openai-codex")
	require.True(t, ok)
	require.Empty(t, provider.Models)

	err = ConfigureSelectedModels(cfg, nil)
	require.NoError(t, err, "a logged-in provider with an unavailable account catalog must not block config loading")
	require.Empty(t, cfg.Models[SelectedModelTypeSmart].Provider)
	require.Empty(t, cfg.Models[SelectedModelTypeSmart].Model)
	require.Empty(t, cfg.Models[SelectedModelTypeFast].Provider)
	require.Empty(t, cfg.Models[SelectedModelTypeFast].Model)
}
