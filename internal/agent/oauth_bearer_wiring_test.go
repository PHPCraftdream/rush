package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/oauth/copilot"
	"github.com/stretchr/testify/require"
)

func oauthBearerStore(t *testing.T, id string, token *oauth.Token) *config.ConfigStore {
	t.Helper()
	store := config.NewLibraryStore(&config.Config{Options: &config.Options{}}, t.TempDir())
	store.SetProviderRuntimeConfig(id, config.ProviderConfig{ID: id, Type: "openai-compat", OAuthToken: token})
	return store
}

func TestOAuthBearerBuilderRotation(t *testing.T) {
	// Revert-check: actual compat builders retain bearer rotation beneath Copilot's outer initiator transport.
	for _, id := range []string{"copilot", "hyper"} {
		t.Run(id, func(t *testing.T) {
			var auth, initiators, payloads []string
			var recorded []http.Header
			headers := map[string]string{"X-Keep": "keep"}
			if id == "copilot" {
				for key, value := range copilot.Headers() {
					headers[key] = value
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				recorded = append(recorded, r.Header.Clone())
				auth = append(auth, r.Header.Get("Authorization"))
				initiators = append(initiators, r.Header.Get("X-Initiator"))
				body, _ := io.ReadAll(r.Body)
				payloads = append(payloads, string(body))
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"test","object":"chat.completion","model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			}))
			defer server.Close()
			store := oauthBearerStore(t, id, &oauth.Token{AccessToken: "first"})
			coord := &coordinator{cfg: store}
			pc, _ := store.Config().Providers.Get(id)
			original := server.Client()
			original.Timeout = 3 * time.Second
			wrapped := coord.withOAuthBearerClient(pc, original)
			require.NotSame(t, original, wrapped)
			require.Equal(t, original.Timeout, wrapped.Timeout)
			require.Same(t, original.Transport, wrapped.Transport.(*oauthBearerTransport).base)
			provider, err := coord.buildOpenaiCompatProvider(store.Config(), server.URL, "static", headers, nil, id, true, wrapped)
			require.NoError(t, err)
			model, err := provider.LanguageModel(t.Context(), "test-model")
			require.NoError(t, err)
			for _, token := range []string{"first", "second"} {
				pc.OAuthToken = &oauth.Token{AccessToken: token}
				store.SetProviderRuntimeConfig(id, pc)
				_, err = model.Generate(t.Context(), fantasy.Call{})
				require.NoError(t, err)
			}
			require.Equal(t, []string{"Bearer first", "Bearer second"}, auth)
			require.Equal(t, payloads[0], payloads[1])
			for _, got := range recorded {
				for key, value := range headers {
					require.Equal(t, value, got.Get(key), "header %s", key)
				}
			}
			if id == "copilot" {
				require.Equal(t, []string{"agent", "agent"}, initiators)
			}
			pc.OAuthToken = nil
			store.SetProviderRuntimeConfig(id, pc)
			oauthLookupStore := &fakeOAuthTokenStore{cfg: store.Config()}
			wrapped.Transport.(*oauthBearerTransport).source.(*oauthTokenSource).store = oauthLookupStore
			_, err = model.Generate(t.Context(), fantasy.Call{})
			require.ErrorContains(t, err, "OAuth credentials unavailable")
			var oauthProviderError *fantasy.ProviderError
			require.True(t, errors.As(err, &oauthProviderError))
			require.Equal(t, http.StatusUnauthorized, oauthProviderError.StatusCode)
			require.Equal(t, 1, oauthLookupStore.reads)
			require.Zero(t, oauthLookupStore.refreshes)
			require.Len(t, auth, 2)
		})
	}
}

func TestOAuthBearerCoordinatorWiring(t *testing.T) {
	// Revert-check: the additive inner-builder helper must install a live source for both OAuth providers.
	for _, id := range []string{"copilot", "hyper"} {
		t.Run(id, func(t *testing.T) {
			store := oauthBearerStore(t, id, &oauth.Token{AccessToken: "memory"})
			coord := &coordinator{cfg: store}
			pc, _ := store.Config().Providers.Get(id)
			provider, err := coord.buildProviderWithValuesInner(store.Config(), pc, config.SelectedModel{}, false, "static", "http://local.invalid", true)
			require.NoError(t, err)
			model, err := provider.LanguageModel(t.Context(), "test-model")
			require.NoError(t, err)
			pc.OAuthToken = nil
			store.SetProviderRuntimeConfig(id, pc)
			_, err = model.Generate(t.Context(), fantasy.Call{})
			require.ErrorContains(t, err, id+" OAuth credentials unavailable")
		})
	}
}

func TestOAuthBearerEligibilityAndClientCopy(t *testing.T) {
	// Revert-check: eligibility leaves API keys, static/nil-store fallback and Codex clients untouched.
	store := oauthBearerStore(t, "copilot", &oauth.Token{AccessToken: "memory"})
	coord := &coordinator{cfg: store}
	original := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, id := range []string{"copilot", "hyper", "openai-codex", "other"} {
		for _, hasToken := range []bool{false, true} {
			pc := config.ProviderConfig{ID: id, APIKey: "key"}
			if hasToken {
				pc.OAuthToken = &oauth.Token{AccessToken: "static"}
			}
			wrapped := coord.withOAuthBearerClient(pc, original)
			if hasToken && (id == "copilot" || id == "hyper") {
				require.NotSame(t, original, wrapped)
				require.Equal(t, original.Timeout, wrapped.Timeout)
				require.ErrorIs(t, wrapped.CheckRedirect(nil, nil), http.ErrUseLastResponse)
				require.Same(t, http.DefaultTransport, wrapped.Transport.(*oauthBearerTransport).base)
			} else {
				require.Same(t, original, wrapped)
			}
			for _, c := range []*coordinator{nil, {}} {
				require.Same(t, original, c.withOAuthBearerClient(pc, original))
				require.Nil(t, c.withOAuthBearerClient(pc, nil))
			}
		}
	}
	require.Nil(t, original.Transport)
	pc := config.ProviderConfig{ID: "copilot", OAuthToken: &oauth.Token{AccessToken: "memory"}}
	wrapped := coord.withOAuthBearerClient(pc, nil)
	require.NotSame(t, http.DefaultClient, wrapped)
	require.IsType(t, &oauthBearerTransport{}, wrapped.Transport)
}

func TestOAuthBearerAPIKeyAndStaticBuilderFallback(t *testing.T) {
	// Revert-check: actual builders without eligible live OAuth preserve SDK API-key/static authorization.
	for _, id := range []string{"copilot", "hyper"} {
		for _, mode := range []string{"apikey", "static"} {
			t.Run(id+"/"+mode, func(t *testing.T) {
				tr := oauthBearerRT(func(r *http.Request) (*http.Response, error) {
					require.Equal(t, "Bearer literal", r.Header.Get("Authorization"))
					response := oauthBearerResponse(200, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
					response.Header.Set("Content-Type", "application/json")
					return response, nil
				})
				store := oauthBearerStore(t, id, nil)
				coord := &coordinator{cfg: store}
				pc, _ := store.Config().Providers.Get(id)
				if mode == "static" {
					coord = &coordinator{}
					pc.OAuthToken = &oauth.Token{AccessToken: "literal"}
				}
				client := &http.Client{Transport: tr}
				wrapped := coord.withOAuthBearerClient(pc, client)
				require.Same(t, client, wrapped)
				provider, err := coord.buildOpenaiCompatProvider(store.Config(), "http://local.invalid", "literal", nil, nil, id, false, wrapped)
				require.NoError(t, err)
				model, err := provider.LanguageModel(t.Context(), "test-model")
				require.NoError(t, err)
				_, err = model.Generate(t.Context(), fantasy.Call{})
				require.NoError(t, err)
			})
		}
	}
}

func TestOAuthBearerCodexUnchanged(t *testing.T) {
	// Revert-check: Codex returns before generic bearer wiring and retains its dedicated account-aware source.
	store := oauthBearerStore(t, "openai-codex", &oauth.Token{AccessToken: "memory", AccountID: "account"})
	coord := &coordinator{cfg: store}
	pc, _ := store.Config().Providers.Get("openai-codex")
	provider, err := coord.buildProviderWithValuesInner(store.Config(), pc, config.SelectedModel{}, false, "", "", true)
	require.NoError(t, err)
	require.Equal(t, "openai-codex", provider.Name())
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	pc.OAuthToken = nil
	store.SetProviderRuntimeConfig(pc.ID, pc)
	_, err = model.Generate(t.Context(), fantasy.Call{})
	require.ErrorContains(t, err, "openai-codex OAuth credentials unavailable")
}

func TestOAuthTokenSourceNestedSnapshot(t *testing.T) {
	// Revert-check: token snapshots and used-token forwarding copy nested client metadata rather than caller pointers.
	token := oauth.Token{AccessToken: "access", Client: &oauth.OAuthClient{ClientID: "original"}}
	store := &fakeOAuthTokenStore{cfg: oauthAdapterConfig(&token)}
	source := &oauthTokenSource{providerID: "copilot", store: store, resolve: func(*config.Config, config.ProviderConfig) (*http.Client, error) { return nil, nil }}
	snapshot, ok := source.Current()
	require.True(t, ok)
	snapshot.Client.ClientID = "snapshot"
	require.Equal(t, "original", token.Client.ClientID)
	store.refresh = func(_ context.Context, _ config.Scope, pc config.ProviderConfig, _ *http.Client) error {
		require.Equal(t, "snapshot", pc.OAuthToken.Client.ClientID)
		pc.OAuthToken.Client.ClientID = "store"
		return nil
	}
	require.NoError(t, source.Refresh(t.Context(), snapshot))
	require.Equal(t, "snapshot", snapshot.Client.ClientID)
}
