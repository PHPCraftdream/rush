package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

type fakeCodexTokenStore struct {
	cfg       *config.Config
	reads     int
	refreshes int
	refresh   func(context.Context, config.Scope, config.ProviderConfig, *http.Client) error
}

func (s *fakeCodexTokenStore) Config() *config.Config { s.reads++; return s.cfg }
func (s *fakeCodexTokenStore) RefreshOAuthTokenWithClient(ctx context.Context, scope config.Scope, pc config.ProviderConfig, client *http.Client) error {
	s.refreshes++
	if s.refresh != nil {
		return s.refresh(ctx, scope, pc, client)
	}
	return nil
}

func codexAdapterConfig(token *oauth.Token) *config.Config {
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})
	store.SetProviderRuntimeConfig(codexTokenProviderID, config.ProviderConfig{ID: codexTokenProviderID, OAuthToken: token})
	return store.Config()
}

func TestCodexTokenSourceCurrentMemoryFreshnessAndCopy(t *testing.T) {
	// Revert-check: removing Current's fresh Config read or token value copy retains stale mutable credentials.
	token := oauth.Token{AccessToken: "first", AccountID: "first-account"}
	store := &fakeCodexTokenStore{cfg: codexAdapterConfig(&token)}
	source := &codexTokenSource{store: store}
	value, ok := source.Current()
	require.True(t, ok)
	value.AccessToken = "caller-change"
	require.Equal(t, "first", token.AccessToken)
	token.AccessToken = "second"
	value, ok = source.Current()
	require.True(t, ok)
	require.Equal(t, "second", value.AccessToken)
	store.cfg = codexAdapterConfig(&oauth.Token{AccessToken: "third"})
	value, ok = source.Current()
	require.True(t, ok)
	require.Equal(t, "third", value.AccessToken)
	require.Equal(t, 3, store.reads)
	require.Zero(t, store.refreshes)
}

func TestCodexTokenSourceMissingAndRemoved(t *testing.T) {
	// Revert-check: removing Current and Refresh's provider-presence guards accepts removed credentials.
	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{{"nil", nil}, {"no-providers", &config.Config{}}, {"removed", config.NewTestStore(&config.Config{Options: &config.Options{}}).Config()}, {"logout", codexAdapterConfig(nil)}} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeCodexTokenStore{cfg: tc.cfg}
			source := &codexTokenSource{store: store, resolve: func(*config.Config, config.ProviderConfig) (*http.Client, error) {
				t.Fatal("must not resolve absent credentials")
				return nil, nil
			}}
			value, ok := source.Current()
			require.False(t, ok)
			require.Empty(t, value.AccessToken)
			require.ErrorContains(t, source.Refresh(t.Context(), oauth.Token{AccessToken: "used"}), "removed or logged out")
			require.Zero(t, store.refreshes)
		})
	}
}

func TestCodexTokenSourceRefreshUsesUsedTokenAndCurrentPolicy(t *testing.T) {
	// Revert-check: removing Refresh's used-token override or ScopeGlobal call exchanges the wrong credentials.
	used := oauth.Token{AccessToken: "used", RefreshToken: "used-refresh", AccountID: "used-account"}
	current := oauth.Token{AccessToken: "current", RefreshToken: "current-refresh"}
	cfg := codexAdapterConfig(&current)
	pc, _ := cfg.Providers.Get(codexTokenProviderID)
	pc.Network = &config.NetworkConfig{Proxy: "http://policy.invalid:8080"}
	pc.Name = "current generation"
	cfg.Providers.Set(codexTokenProviderID, pc)
	store := &fakeCodexTokenStore{cfg: cfg}
	client := &http.Client{}
	resolved := 0
	source := &codexTokenSource{store: store, resolve: func(got *config.Config, entry config.ProviderConfig) (*http.Client, error) {
		resolved++
		require.Same(t, cfg, got)
		require.Same(t, &current, entry.OAuthToken)
		require.Equal(t, pc.Network, entry.Network)
		store.cfg = codexAdapterConfig(&oauth.Token{AccessToken: "later"})
		return client, nil
	}}
	store.refresh = func(ctx context.Context, scope config.Scope, entry config.ProviderConfig, got *http.Client) error {
		require.Same(t, t.Context(), ctx)
		require.Equal(t, config.ScopeGlobal, scope)
		require.Same(t, client, got)
		require.Equal(t, used, *entry.OAuthToken)
		require.NotSame(t, &used, entry.OAuthToken)
		require.Equal(t, pc.Network, entry.Network)
		require.Equal(t, pc.Name, entry.Name)
		entry.OAuthToken.AccessToken = "store-local-copy"
		return nil
	}
	require.NoError(t, source.Refresh(t.Context(), used))
	require.Equal(t, "used", used.AccessToken)
	require.Equal(t, "current", current.AccessToken)
	require.Equal(t, 1, store.reads)
	require.Equal(t, 1, resolved)
	require.Equal(t, 1, store.refreshes)
}

func TestCodexTokenSourceResolverFailureRefusesRefresh(t *testing.T) {
	// Revert-check: removing Refresh's resolver-error guard falls back to an unauthorized default route.
	store := &fakeCodexTokenStore{cfg: codexAdapterConfig(&oauth.Token{AccessToken: "current"})}
	failure := errors.New("policy resolution failed")
	source := &codexTokenSource{store: store, resolve: func(*config.Config, config.ProviderConfig) (*http.Client, error) { return nil, failure }}
	require.ErrorIs(t, source.Refresh(t.Context(), oauth.Token{AccessToken: "used"}), failure)
	require.Zero(t, store.refreshes)
}

func TestCodexTokenSourceDefaultPolicyAndStoreError(t *testing.T) {
	// Revert-check: removing Refresh's direct store call loses the nil default-policy client or store error.
	store := &fakeCodexTokenStore{cfg: codexAdapterConfig(&oauth.Token{AccessToken: "current"})}
	failure := errors.New("store rejected refresh")
	source := &codexTokenSource{store: store, resolve: func(*config.Config, config.ProviderConfig) (*http.Client, error) { return nil, nil }}
	store.refresh = func(_ context.Context, scope config.Scope, _ config.ProviderConfig, client *http.Client) error {
		require.Equal(t, config.ScopeGlobal, scope)
		require.Nil(t, client)
		return failure
	}
	require.ErrorIs(t, source.Refresh(t.Context(), oauth.Token{AccessToken: "used"}), failure)
	require.Equal(t, 1, store.refreshes)
}

func TestCodexTokenSourceRefreshCancellation(t *testing.T) {
	// Revert-check: removing Refresh's context guards invokes the store after cancellation.
	for _, when := range []string{"before", "resolver", "store"} {
		t.Run(when, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := &fakeCodexTokenStore{cfg: codexAdapterConfig(&oauth.Token{AccessToken: "current"})}
			resolved := 0
			source := &codexTokenSource{store: store, resolve: func(*config.Config, config.ProviderConfig) (*http.Client, error) {
				resolved++
				if when == "resolver" {
					cancel()
				}
				return nil, nil
			}}
			store.refresh = func(got context.Context, _ config.Scope, _ config.ProviderConfig, _ *http.Client) error {
				cancel()
				<-got.Done()
				return got.Err()
			}
			if when == "before" {
				cancel()
			}
			require.ErrorIs(t, source.Refresh(ctx, oauth.Token{AccessToken: "used"}), context.Canceled)
			if when == "before" {
				require.Zero(t, resolved)
				require.Zero(t, store.reads)
			}
			if when == "store" {
				require.Equal(t, 1, store.refreshes)
			} else {
				require.Zero(t, store.refreshes)
			}
		})
	}
}

type codexAdapterRoundTripper func(*http.Request) (*http.Response, error)

func (r codexAdapterRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return r(request)
}

func TestCodexTokenSourceRealStoreMemoryRotation(t *testing.T) {
	// Revert-check: replacing the adapter's fresh Current read with a cached token retains the first token.
	store := config.NewLibraryStore(&config.Config{Options: &config.Options{}}, t.TempDir())
	old := oauth.Token{AccessToken: "old-memory", AccountID: "old-account"}
	store.SetProviderRuntimeConfig(codexTokenProviderID, config.ProviderConfig{ID: codexTokenProviderID, OAuthToken: &old})
	coord := &coordinator{cfg: store}
	pc, _ := store.Config().Providers.Get(codexTokenProviderID)
	var auth, accounts []string
	client := &http.Client{Transport: codexAdapterRoundTripper(func(r *http.Request) (*http.Response, error) {
		auth = append(auth, r.Header.Get("Authorization"))
		accounts = append(accounts, r.Header.Get("chatgpt-account-id"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}, nil
	})}
	source := &codexTokenSource{store: store, resolve: coord.resolveProviderHTTPClient}
	provider, err := newCodexTokenProvider(client, source)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	_, err = model.Generate(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	newer := oauth.Token{AccessToken: "new-memory", AccountID: "new-account"}
	pc.OAuthToken = &newer
	store.SetProviderRuntimeConfig(codexTokenProviderID, pc)
	_, err = model.Generate(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	require.Equal(t, []string{"Bearer old-memory", "Bearer new-memory"}, auth)
	require.Equal(t, []string{"old-account", "new-account"}, accounts)
}

func TestCodexTokenSourceCoordinatorStaticFallback(t *testing.T) {
	// Revert-check: removing the Codex branch's nil-store fallback rejects lightweight coordinators.
	token := oauth.Token{AccessToken: "static"}
	cfg := &config.Config{Options: &config.Options{}}
	pc := config.ProviderConfig{ID: codexTokenProviderID, OAuthToken: &token}
	for _, coord := range []*coordinator{nil, {}} {
		provider, err := coord.buildProviderWithValuesInner(cfg, pc, config.SelectedModel{}, false, "", "", true)
		require.NoError(t, err)
		require.Equal(t, codexTokenProviderID, provider.Name())
	}
}

func TestCodexTokenSourceCoordinatorUsesLiveSource(t *testing.T) {
	// Revert-check: removing the Codex branch's source-backed constructor freezes logged-out credentials.
	token := oauth.Token{AccessToken: "memory-only"}
	store := config.NewLibraryStore(codexAdapterConfig(&token), t.TempDir())
	coord := &coordinator{cfg: store}
	pc, _ := store.Config().Providers.Get(codexTokenProviderID)
	provider, err := coord.buildProvider(store.Config(), pc, config.SelectedModel{}, false)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	pc.OAuthToken = nil
	store.SetProviderRuntimeConfig(codexTokenProviderID, pc)
	_, err = model.Generate(t.Context(), fantasy.Call{})
	require.EqualError(t, err, "openai-codex OAuth credentials unavailable: provider removed, logged out, or access token empty")
}

func TestCodexTokenSourceConstructorRequiresOAuth(t *testing.T) {
	// Revert-check: removing newCodexTokenProvider's initial credential guard accepts API-key-only configuration.
	_, err := newCodexTokenProvider(nil, &codexTokenSource{store: &fakeCodexTokenStore{cfg: codexAdapterConfig(nil)}})
	require.ErrorContains(t, err, "OAuth access token")
	var _ codexprovider.TokenSource = (*codexTokenSource)(nil)
}
