package config

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestRefreshOAuthToken_CodexRotatesAndPreservesAccountWithoutAPIKey(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	oldToken := &oauth.Token{
		AccessToken:  "codex-old-access",
		RefreshToken: "codex-old-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
		AccountID:    "account-bound-id",
	}
	content, err := json.Marshal(map[string]any{
		"providers": map[string]any{
			"openai-codex": map[string]any{"api_key": "stale-codex-key", "oauth": oldToken},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, content, 0o600))
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("openai-codex", ProviderConfig{ID: "openai-codex", OAuthToken: oldToken})
	store := newTestConfigStore(testStoreOpts{config: &Config{Providers: providers}, globalDataPath: configPath})

	originalRefresh := codexRefreshTokenFn
	codexRefreshTokenFn = func(_ context.Context, _ *http.Client, refreshToken string) (*oauth.Token, error) {
		require.Equal(t, oldToken.RefreshToken, refreshToken)
		return &oauth.Token{
			AccessToken:  "codex-new-access",
			RefreshToken: "codex-rotated-refresh",
			ExpiresIn:    3600,
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		}, nil
	}
	t.Cleanup(func() { codexRefreshTokenFn = originalRefresh })

	require.NoError(t, store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, ProviderConfig{
		ID:         "openai-codex",
		OAuthToken: oldToken,
	}, nil))

	current, exists := store.loadSnapshot().config.Providers.Get("openai-codex")
	require.True(t, exists)
	require.Equal(t, "codex-new-access", current.OAuthToken.AccessToken)
	require.Equal(t, "codex-rotated-refresh", current.OAuthToken.RefreshToken)
	require.Equal(t, "account-bound-id", current.OAuthToken.AccountID)
	require.Empty(t, current.APIKey)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var persisted struct {
		Providers map[string]struct {
			APIKey string      `json:"api_key"`
			OAuth  oauth.Token `json:"oauth"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(data, &persisted))
	codexProvider := persisted.Providers["openai-codex"]
	require.Empty(t, codexProvider.APIKey)
	require.Equal(t, "codex-new-access", codexProvider.OAuth.AccessToken)
	require.Equal(t, "codex-rotated-refresh", codexProvider.OAuth.RefreshToken)
	require.Equal(t, "account-bound-id", codexProvider.OAuth.AccountID)
}
