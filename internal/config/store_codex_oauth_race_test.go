package config

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestRefreshOAuthToken_CodexConcurrentDiskRotationWins(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	oldToken := &oauth.Token{
		AccessToken:  "old-codex-access",
		RefreshToken: "old-codex-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
		AccountID:    "chatgpt-account",
	}
	replacement := &oauth.Token{
		AccessToken:  "other-session-access",
		RefreshToken: "other-session-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		AccountID:    "chatgpt-account",
	}
	writeConfig := func(token *oauth.Token) []byte {
		content, err := json.Marshal(map[string]any{
			"providers": map[string]any{
				"openai-codex": map[string]any{"oauth": token},
			},
		})
		require.NoError(t, err)
		return content
	}
	require.NoError(t, os.WriteFile(configPath, writeConfig(oldToken), 0o600))
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("openai-codex", ProviderConfig{ID: "openai-codex", OAuthToken: oldToken})
	store := newTestConfigStore(testStoreOpts{config: &Config{Providers: providers}, globalDataPath: configPath})

	requestArrived := make(chan struct{})
	releaseResponse := make(chan struct{})
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse refresh request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != oldToken.RefreshToken {
			t.Errorf("unexpected refresh request: %v", r.Form)
		}
		close(requestArrived)
		<-releaseResponse
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&oauth.Token{
			AccessToken:  "stale-refresh-result",
			RefreshToken: "stale-refresh-rotation",
			ExpiresIn:    3600,
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		})
	}))
	defer tokenEndpoint.Close()

	originalRefresh := codexRefreshTokenFn
	codexRefreshTokenFn = func(ctx context.Context, client *http.Client, refreshToken string) (*oauth.Token, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint.URL, strings.NewReader(url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {refreshToken},
		}.Encode()))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		var token oauth.Token
		if err := json.NewDecoder(response.Body).Decode(&token); err != nil {
			return nil, err
		}
		return &token, nil
	}
	t.Cleanup(func() { codexRefreshTokenFn = originalRefresh })

	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, ProviderConfig{
			ID:         "openai-codex",
			OAuthToken: oldToken,
		}, tokenEndpoint.Client())
	}()
	select {
	case <-requestArrived:
	case <-time.After(10 * time.Second):
		t.Fatal("Codex refresh did not reach the token endpoint")
	}

	// Model another Rush process committing a replacement while this process
	// waits for the authorization server response.
	require.NoError(t, os.WriteFile(configPath, writeConfig(replacement), 0o600))
	close(releaseResponse)
	select {
	case err := <-refreshDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Codex refresh did not reconcile concurrent disk rotation")
	}

	current, exists := store.loadSnapshot().config.Providers.Get("openai-codex")
	require.True(t, exists)
	require.Equal(t, replacement.AccessToken, current.OAuthToken.AccessToken)
	require.Equal(t, replacement.RefreshToken, current.OAuthToken.RefreshToken)
	require.Equal(t, "chatgpt-account", current.OAuthToken.AccountID)
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
	require.Equal(t, replacement.AccessToken, codexProvider.OAuth.AccessToken)
	require.Equal(t, replacement.RefreshToken, codexProvider.OAuth.RefreshToken)
}
