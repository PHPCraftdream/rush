package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/oauth/copilot"
	"github.com/stretchr/testify/require"
)

// task1314CountingStore forwards both operations to the real store.
// Refresh entries are counted, not exchanges inside ConfigStore.
type task1314CountingStore struct {
	*config.ConfigStore
	refreshes atomic.Int32
}

func (s *task1314CountingStore) RefreshOAuthTokenWithClient(ctx context.Context, scope config.Scope, pc config.ProviderConfig, client *http.Client) error {
	s.refreshes.Add(1)
	return s.ConfigStore.RefreshOAuthTokenWithClient(ctx, scope, pc, client)
}

func TestTask1314OAuthMemoryRotationFourPrebuiltClients(t *testing.T) {
	// Revert-check: codexTokenSource.Current, oauthTokenSource.Current and oauthBearerTransport.RoundTrip must read live credentials for every prebuilt client.
	t.Parallel()
	for _, id := range []string{"openai-codex", "copilot", "hyper"} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			old := &oauth.Token{AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh", AccountID: "fixture-old-account", ExpiresAt: time.Now().Add(time.Hour).Unix()}
			next := &oauth.Token{AccessToken: "fixture-new-access", RefreshToken: "fixture-new-refresh", AccountID: "fixture-new-account", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()}
			store := &task1314CountingStore{ConfigStore: oauthBearerStore(t, id, old)}
			coord := &coordinator{cfg: store.ConfigStore}
			var good atomic.Pointer[oauth.Token]
			good.Store(old)
			var sends [2][4]atomic.Int32
			var rejected, unexpected, exchangeRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/backend-api/codex/responses" {
					exchangeRequests.Add(1)
					http.Error(w, "unexpected endpoint", http.StatusBadRequest)
					return
				}
				client, err := strconv.Atoi(r.Header.Get("X-Task1314-Client"))
				if err != nil || client < 0 || client >= 4 {
					unexpected.Add(1)
					http.Error(w, "unknown client", http.StatusBadRequest)
					return
				}
				want := good.Load()
				phase := 0
				if want == next {
					phase = 1
				}
				sends[phase][client].Add(1)
				if r.Header.Get("Authorization") != "Bearer "+want.AccessToken || id == "openai-codex" && r.Header.Get("chatgpt-account-id") != want.AccountID {
					rejected.Add(1)
					http.Error(w, "rejected credentials", http.StatusUnauthorized)
					return
				}
				if id == "copilot" {
					initiator := "agent"
					if client == 0 {
						initiator = "user"
					}
					if r.Header.Get("X-Initiator") != initiator {
						unexpected.Add(1)
					}
				}
				if id == "openai-codex" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"fixture","object":"chat.completion","model":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			}))
			t.Cleanup(server.Close)
			endpoint, err := url.Parse(server.URL)
			require.NoError(t, err)
			pc, ok := store.Config().Providers.Get(id)
			require.True(t, ok)
			var models [4]fantasy.LanguageModel
			for index := range models {
				// Only URL routing and a client label are test-specific; HTTP is real.
				base := server.Client().Transport.(*http.Transport).Clone()
				t.Cleanup(base.CloseIdleConnections)
				client := &http.Client{Timeout: 3 * time.Second, Transport: codexAdapterRoundTripper(func(r *http.Request) (*http.Response, error) {
					forwarded := r.Clone(r.Context())
					forwarded.URL.Scheme, forwarded.URL.Host = endpoint.Scheme, endpoint.Host
					forwarded.Host = endpoint.Host
					forwarded.Header.Set("X-Task1314-Client", strconv.Itoa(index))
					return base.RoundTrip(forwarded)
				})}
				var provider fantasy.Provider
				if id == "openai-codex" {
					provider, err = newCodexTokenProvider(client, &codexTokenSource{store: store, resolve: coord.resolveProviderHTTPClient})
				} else {
					wrapped := coord.withOAuthBearerClient(pc, client)
					// Count refresh calls without replacing the production source.
					wrapped.Transport.(*oauthBearerTransport).source.(*oauthTokenSource).store = store
					var headers map[string]string
					if id == "copilot" {
						headers = copilot.Headers()
					}
					provider, err = coord.buildOpenaiCompatProvider(store.Config(), server.URL+"/v1", old.AccessToken, headers, nil, id, index != 0, wrapped)
				}
				require.NoError(t, err)
				models[index], err = provider.LanguageModel(t.Context(), "gpt-5-codex")
				require.NoError(t, err)
			}
			// All four clients/models exist before either phase and are never rebuilt.
			for phase, token := range []*oauth.Token{old, next} {
				if phase == 1 {
					pc.OAuthToken = token
					store.SetProviderRuntimeConfig(id, pc)
					good.Store(token)
				}
				for index, model := range models {
					ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
					_, err := model.Generate(ctx, fantasy.Call{})
					cancel()
					require.NoError(t, err)
					require.EqualValues(t, 1, sends[phase][index].Load(), "phase %d client %d", phase, index)
				}
			}
			require.Zero(t, rejected.Load(), "no request may require a 401 refresh")
			require.Zero(t, unexpected.Load())
			require.Zero(t, exchangeRequests.Load(), "no exchange endpoint may be requested")
			require.Zero(t, store.refreshes.Load(), "zero real-store Refresh entries implies zero exchanges through that path")
		})
	}
}
