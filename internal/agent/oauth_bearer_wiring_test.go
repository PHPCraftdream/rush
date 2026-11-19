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
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth/copilot"
	"github.com/stretchr/testify/require"
)

func oauthBearerStore(t *testing.T, id string, token *oauth.Token) *config.ConfigStore {
	t.Helper()
	store := config.NewLibraryStore(&config.Config{Options: &config.Options{}}, t.TempDir())
	store.SetProviderRuntimeConfig(id, config.ProviderConfig{ID: id, Type: "openai-compat", OAuthToken: token})
	return store
}

// countedLiveOAuthStore counts provauth token-source lookups while always delegating to the live
// store: ConfigStore.Config() returns snapshot pointers and SetProviderRuntimeConfig publishes a
// fresh config copy, so caching cfg once would go stale across token rotation and logout.
type countedLiveOAuthStore struct {
	store     *config.ConfigStore
	reads     int
	refreshes int
}

func (s *countedLiveOAuthStore) Config() *config.Config { s.reads++; return s.store.Config() }
func (s *countedLiveOAuthStore) RefreshOAuthTokenWithClient(ctx context.Context, scope config.Scope, pc config.ProviderConfig, client *http.Client) error {
	s.refreshes++
	return s.store.RefreshOAuthTokenWithClient(ctx, scope, pc, client)
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
			require.Same(t, original.Transport, wrapped.Transport.(*provauth.BearerTransport).Base())
			// Install a counted live source BEFORE provider construction: Copilot copies/retains the
			// inner transport at build time, so a post-build rewrap of `wrapped` never reaches the model.
			// The counted store delegates Config() to the live store on every call: store.Config()
			// returns a snapshot pointer and SetProviderRuntimeConfig publishes a fresh copy, so a
			// cached cfg would go stale across token rotation and logout.
			oauthLookupStore := &countedLiveOAuthStore{store: store}
			tr := wrapped.Transport.(*provauth.BearerTransport)
			wrapped.Transport = provauth.NewBearerTransport(tr.Base(), provauth.NewOAuthTokenSource(id, oauthLookupStore, coord.resolveProviderHTTPClient), id)
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
			readsBeforeLogout := oauthLookupStore.reads
			_, err = model.Generate(t.Context(), fantasy.Call{})
			require.ErrorContains(t, err, "OAuth credentials unavailable")
			var oauthProviderError *fantasy.ProviderError
			require.True(t, errors.As(err, &oauthProviderError))
			require.Equal(t, http.StatusUnauthorized, oauthProviderError.StatusCode)
			require.Equal(t, readsBeforeLogout+1, oauthLookupStore.reads)
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
				require.Same(t, http.DefaultTransport, wrapped.Transport.(*provauth.BearerTransport).Base())
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
	var wantTransport *provauth.BearerTransport
	require.IsType(t, wantTransport, wrapped.Transport)
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

func TestOAuthBearerBuilderLoggedOut1302(t *testing.T) {
	store := oauthBearerStore(t, "copilot", &oauth.Token{AccessToken: "synthetic"})
	coord := &coordinator{cfg: store}
	pc, ok := store.Config().Providers.Get("copilot")
	require.True(t, ok)
	sends := 0
	client := &http.Client{Transport: oauthBearerRT(func(*http.Request) (*http.Response, error) {
		sends++
		return nil, errors.New("unexpected outbound send")
	})}
	wrapped := coord.withOAuthBearerClient(pc, client)
	require.NotSame(t, client, wrapped)
	source := &rootOAuthBearerSource{token: oauth.Token{AccessToken: "old"}, present: true}
	tr := wrapped.Transport.(*provauth.BearerTransport)
	wrapped.Transport = provauth.NewBearerTransport(tr.Base(), source, pc.ID)
	provider, err := coord.buildOpenaiCompatProvider(store.Config(), "http://local.invalid", "synthetic", nil, nil, pc.ID, true, wrapped)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	model, err := provider.LanguageModel(ctx, "test-model")
	require.NoError(t, err)
	source.set(oauth.Token{}, false)
	start := time.Now()
	_, err = model.Generate(ctx, fantasy.Call{})
	elapsed := time.Since(start)
	require.ErrorContains(t, err, "copilot OAuth credentials unavailable")
	var providerErr *fantasy.ProviderError
	require.True(t, errors.As(err, &providerErr))
	require.Equal(t, http.StatusUnauthorized, providerErr.StatusCode)
	require.EqualValues(t, 1, source.lookups.Load())
	require.Zero(t, source.refreshes.Load())
	require.Zero(t, sends)
	require.Less(t, elapsed, 2*time.Second)
}
