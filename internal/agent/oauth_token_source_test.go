package agent

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

type fakeOAuthTokenStore struct {
	cfg       *config.Config
	reads     int
	refreshes int
	refresh   func(context.Context, config.Scope, config.ProviderConfig, *http.Client) error
}

func (s *fakeOAuthTokenStore) Config() *config.Config { s.reads++; return s.cfg }
func (s *fakeOAuthTokenStore) RefreshOAuthTokenWithClient(ctx context.Context, scope config.Scope, pc config.ProviderConfig, client *http.Client) error {
	s.refreshes++
	if s.refresh != nil {
		return s.refresh(ctx, scope, pc, client)
	}
	return nil
}

func oauthAdapterConfig(token *oauth.Token) *config.Config {
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})
	store.SetProviderRuntimeConfig("copilot", config.ProviderConfig{ID: "copilot", OAuthToken: token})
	return store.Config()
}

func TestOAuthTokenSourceCurrentMemoryFreshnessAndCopy(t *testing.T) {
	// Revert-check: removing Current's fresh Config read or token value copy retains stale mutable credentials.
	token := oauth.Token{AccessToken: "first", AccountID: "first-account"}
	store := &fakeOAuthTokenStore{cfg: oauthAdapterConfig(&token)}
	source := &oauthTokenSource{providerID: "copilot", store: store}
	value, ok := source.Current()
	require.True(t, ok)
	value.AccessToken = "caller-change"
	require.Equal(t, "first", token.AccessToken)
	token.AccessToken = "second"
	value, ok = source.Current()
	require.True(t, ok)
	require.Equal(t, "second", value.AccessToken)
	store.cfg = oauthAdapterConfig(&oauth.Token{AccessToken: "third"})
	value, ok = source.Current()
	require.True(t, ok)
	require.Equal(t, "third", value.AccessToken)
	require.Equal(t, 3, store.reads)
	require.Zero(t, store.refreshes)
}

func TestOAuthTokenSourceMissingAndRemoved(t *testing.T) {
	// Revert-check: removing Current and Refresh's provider-presence guards accepts removed credentials.
	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{{"nil", nil}, {"no-providers", &config.Config{}}, {"removed", config.NewTestStore(&config.Config{Options: &config.Options{}}).Config()}, {"logout", oauthAdapterConfig(nil)}} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeOAuthTokenStore{cfg: tc.cfg}
			source := &oauthTokenSource{providerID: "copilot", store: store, resolve: func(*config.Config, config.ProviderConfig) (*http.Client, error) {
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

func TestOAuthTokenSourceRefreshUsesUsedTokenAndCurrentPolicy(t *testing.T) {
	// Revert-check: removing Refresh's used-token override or ScopeGlobal call exchanges the wrong credentials.
	used := oauth.Token{AccessToken: "used", RefreshToken: "used-refresh", AccountID: "used-account"}
	current := oauth.Token{AccessToken: "current", RefreshToken: "current-refresh"}
	cfg := oauthAdapterConfig(&current)
	pc, _ := cfg.Providers.Get("copilot")
	pc.Network = &config.NetworkConfig{Proxy: "http://policy.invalid:8080"}
	pc.Name = "current generation"
	cfg.Providers.Set("copilot", pc)
	store := &fakeOAuthTokenStore{cfg: cfg}
	client := &http.Client{}
	resolved := 0
	source := &oauthTokenSource{providerID: "copilot", store: store, resolve: func(got *config.Config, entry config.ProviderConfig) (*http.Client, error) {
		resolved++
		require.Same(t, cfg, got)
		require.Same(t, &current, entry.OAuthToken)
		require.Equal(t, pc.Network, entry.Network)
		store.cfg = oauthAdapterConfig(&oauth.Token{AccessToken: "later"})
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

func TestOAuthTokenSourceResolverFailureRefusesRefresh(t *testing.T) {
	// Revert-check: removing Refresh's resolver-error guard falls back to an unauthorized default route.
	store := &fakeOAuthTokenStore{cfg: oauthAdapterConfig(&oauth.Token{AccessToken: "current"})}
	failure := errors.New("policy resolution failed")
	source := &oauthTokenSource{providerID: "copilot", store: store, resolve: func(*config.Config, config.ProviderConfig) (*http.Client, error) { return nil, failure }}
	require.ErrorIs(t, source.Refresh(t.Context(), oauth.Token{AccessToken: "used"}), failure)
	require.Zero(t, store.refreshes)
}

func TestOAuthTokenSourceDefaultPolicyAndStoreError(t *testing.T) {
	// Revert-check: removing Refresh's direct store call loses the nil default-policy client or store error.
	store := &fakeOAuthTokenStore{cfg: oauthAdapterConfig(&oauth.Token{AccessToken: "current"})}
	failure := errors.New("store rejected refresh")
	source := &oauthTokenSource{providerID: "copilot", store: store, resolve: func(*config.Config, config.ProviderConfig) (*http.Client, error) { return nil, nil }}
	store.refresh = func(_ context.Context, scope config.Scope, _ config.ProviderConfig, client *http.Client) error {
		require.Equal(t, config.ScopeGlobal, scope)
		require.Nil(t, client)
		return failure
	}
	require.ErrorIs(t, source.Refresh(t.Context(), oauth.Token{AccessToken: "used"}), failure)
	require.Equal(t, 1, store.refreshes)
}

func TestOAuthTokenSourceRefreshCancellation(t *testing.T) {
	// Revert-check: removing Refresh's context guards invokes the store after cancellation.
	for _, when := range []string{"before", "resolver", "store"} {
		t.Run(when, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := &fakeOAuthTokenStore{cfg: oauthAdapterConfig(&oauth.Token{AccessToken: "current"})}
			resolved := 0
			source := &oauthTokenSource{providerID: "copilot", store: store, resolve: func(*config.Config, config.ProviderConfig) (*http.Client, error) {
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
