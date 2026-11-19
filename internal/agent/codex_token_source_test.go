package agent

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/stretchr/testify/require"
)

type codexAdapterRoundTripper func(*http.Request) (*http.Response, error)

func (r codexAdapterRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return r(request)
}

func TestCodexTokenSourceRealStoreMemoryRotation(t *testing.T) {
	// Revert-check: replacing the adapter's fresh Current read with a cached token retains the first token.
	store := config.NewLibraryStore(&config.Config{Options: &config.Options{}}, t.TempDir())
	old := oauth.Token{AccessToken: "old-memory", AccountID: "old-account"}
	store.SetProviderRuntimeConfig(provauth.CodexTokenProviderID, config.ProviderConfig{ID: provauth.CodexTokenProviderID, OAuthToken: &old})
	coord := &coordinator{cfg: store}
	pc, _ := store.Config().Providers.Get(provauth.CodexTokenProviderID)
	var auth, accounts []string
	client := &http.Client{Transport: codexAdapterRoundTripper(func(r *http.Request) (*http.Response, error) {
		auth = append(auth, r.Header.Get("Authorization"))
		accounts = append(accounts, r.Header.Get("chatgpt-account-id"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}, nil
	})}
	source := provauth.NewCodexTokenSource(store, coord.resolveProviderHTTPClient)
	provider, err := provauth.NewCodexTokenProvider(client, source)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	_, err = model.Generate(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	newer := oauth.Token{AccessToken: "new-memory", AccountID: "new-account"}
	pc.OAuthToken = &newer
	store.SetProviderRuntimeConfig(provauth.CodexTokenProviderID, pc)
	_, err = model.Generate(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	require.Equal(t, []string{"Bearer old-memory", "Bearer new-memory"}, auth)
	require.Equal(t, []string{"old-account", "new-account"}, accounts)
}

func TestCodexTokenSourceCoordinatorStaticFallback(t *testing.T) {
	// Revert-check: removing the Codex branch's nil-store fallback rejects lightweight coordinators.
	token := oauth.Token{AccessToken: "static"}
	cfg := &config.Config{Options: &config.Options{}}
	pc := config.ProviderConfig{ID: provauth.CodexTokenProviderID, OAuthToken: &token}
	for _, coord := range []*coordinator{nil, {}} {
		provider, err := coord.buildProviderWithValuesInner(cfg, pc, config.SelectedModel{}, false, "", "", true)
		require.NoError(t, err)
		require.Equal(t, provauth.CodexTokenProviderID, provider.Name())
	}
}

func TestCodexTokenSourceCoordinatorUsesLiveSource(t *testing.T) {
	// Revert-check: removing the Codex branch's source-backed constructor freezes logged-out credentials.
	token := oauth.Token{AccessToken: "memory-only"}
	base := config.NewTestStore(&config.Config{Options: &config.Options{}})
	base.SetProviderRuntimeConfig(provauth.CodexTokenProviderID, config.ProviderConfig{ID: provauth.CodexTokenProviderID, OAuthToken: &token})
	store := config.NewLibraryStore(base.Config(), t.TempDir())
	coord := &coordinator{cfg: store}
	pc, _ := store.Config().Providers.Get(provauth.CodexTokenProviderID)
	provider, err := coord.buildProvider(store.Config(), pc, config.SelectedModel{}, false)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	pc.OAuthToken = nil
	store.SetProviderRuntimeConfig(provauth.CodexTokenProviderID, pc)
	_, err = model.Generate(t.Context(), fantasy.Call{})
	require.EqualError(t, err, "openai-codex OAuth credentials unavailable: provider removed, logged out, or access token empty")
}
