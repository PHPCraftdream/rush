package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

var task1314IntegrationIDs = []string{"openai-codex", "copilot", "hyper"}

func task1314IntegrationSubprocess(t *testing.T, child, key string) {
	t.Helper()
	root := t.TempDir()
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := platform.Command(ctx, executable, "-test.run=^"+child+"$", "-test.v", "-test.parallel=1")
	cmd.Env = append(os.Environ(), key+"="+root)
	output, err := cmd.CombinedOutput()
	t.Logf("Child output:\n%s", output)
	require.NoError(t, err)
	// TestMain does not propagate m.Run's status: only a successful child marks completion.
	_, err = os.Stat(filepath.Join(root, "complete"))
	require.NoError(t, err, "child assertions failed: %s", output)
}

func TestTask1314OAuthDiskIntegrationSubprocess(t *testing.T) {
	// Revert-check: real disk adoption and concurrent reactive retries must survive public Load and production provider wiring.
	task1314IntegrationSubprocess(t, "TestTask1314OAuthDiskIntegrationChild", "TASK1314_INTEGRATION_CHILD")
}

func task1314IntegrationTokens() (*oauth.Token, *oauth.Token) {
	return &oauth.Token{AccessToken: "fixture-old", RefreshToken: "fixture-old-refresh", AccountID: "fixture-old-account", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		&oauth.Token{AccessToken: "fixture-next", RefreshToken: "fixture-next-refresh", AccountID: "fixture-next-account", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()}
}

func task1314IntegrationSeed(store *config.ConfigStore, id string, token *oauth.Token) {
	store.SetProviderRuntimeConfig(id, config.ProviderConfig{ID: id, Type: "openai-compat", APIKey: token.AccessToken, OAuthToken: token})
}

func task1314IntegrationWriteAll(t *testing.T, path string, token *oauth.Token) {
	t.Helper()
	providers := map[string]any{}
	for _, id := range task1314IntegrationIDs {
		providers[id] = map[string]any{"oauth": token}
	}
	data, err := json.Marshal(map[string]any{"providers": providers})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// Track every response body returned by the real network transport, including 401 and exchange bodies.
type (
	task1314IntegrationBodies struct{ opened, closed atomic.Int32 }
	task1314IntegrationBody   struct {
		io.ReadCloser
		once   sync.Once
		counts *task1314IntegrationBodies
	}
)

func (b *task1314IntegrationBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.counts.closed.Add(1) })
	return err
}

func task1314IntegrationModel(t *testing.T, store *task1314CountingStore, id string, server *httptest.Server, bodies *task1314IntegrationBodies) fantasy.LanguageModel {
	t.Helper()
	endpoint, err := url.Parse(server.URL)
	require.NoError(t, err)
	base := server.Client().Transport.(*http.Transport).Clone()
	t.Cleanup(base.CloseIdleConnections)
	client := &http.Client{Timeout: 10 * time.Second, Transport: codexAdapterRoundTripper(func(r *http.Request) (*http.Response, error) {
		forwarded := r.Clone(r.Context())
		forwarded.URL.Scheme, forwarded.URL.Host = endpoint.Scheme, endpoint.Host
		forwarded.Host = endpoint.Host
		forwarded.Header = r.Header.Clone()
		forwarded.Header.Set("X-Task1314-Provider", id)
		response, err := base.RoundTrip(forwarded)
		if response != nil && response.Body != nil {
			bodies.opened.Add(1)
			response.Body = &task1314IntegrationBody{ReadCloser: response.Body, counts: bodies}
		}
		return response, err
	})}
	coord := &coordinator{cfg: store.ConfigStore}
	resolve := func(*config.Config, config.ProviderConfig) (*http.Client, error) { return client, nil }
	pc, ok := store.Config().Providers.Get(id)
	require.True(t, ok)
	var provider fantasy.Provider
	if id == "openai-codex" {
		provider, err = newCodexTokenProvider(client, &codexTokenSource{store: store, resolve: resolve})
	} else {
		wrapped := coord.withOAuthBearerClient(pc, client)
		source := wrapped.Transport.(*oauthBearerTransport).source.(*oauthTokenSource)
		source.store, source.resolve = store, resolve
		provider, err = coord.buildOpenaiCompatProvider(store.Config(), server.URL+"/v1", pc.APIKey, nil, nil, id, false, wrapped)
	}
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	return model
}

func task1314IntegrationRequest(r *http.Request) bool {
	return r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/backend-api/codex/responses"
}

func task1314IntegrationCredentials(r *http.Request, id string, token *oauth.Token) bool {
	return r.Header.Get("Authorization") == "Bearer "+token.AccessToken && (id != "openai-codex" || r.Header.Get("chatgpt-account-id") == token.AccountID)
}

func task1314IntegrationReply(w http.ResponseWriter, id string, unauthorized bool) {
	if unauthorized {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"fixture-original-401","type":"authentication_error"}}`)
	} else if id == "openai-codex" {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	} else {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}
}

func TestTask1314OAuthDiskIntegrationChild(t *testing.T) {
	// Revert-check: real-clock polling and four independently built clients must use live adopted disk credentials.
	root := os.Getenv("TASK1314_INTEGRATION_CHILD")
	if root == "" {
		t.Skip("subprocess helper")
	}
	// The child is a separate process: it inherits nothing from the
	// parent's package variables, so the gate must be disabled here too.
	// This is a legacy-JSON fixture: it rewrites rush.json at the literal
	// GlobalConfigData path and expects the reactive disk watcher to see it.
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)

	t.Run("realclock-poller-all3", func(t *testing.T) {
		// Revert-check: removing StartOAuthDiskSync's real worker leaves all three prebuilt models stale without a reactive 401.
		store, path := task1314LoadDiskStore(t)
		old, next := task1314IntegrationTokens()
		counted := &task1314CountingStore{ConfigStore: store}
		var sends, exchanges atomic.Int32
		var bodies task1314IntegrationBodies
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !task1314IntegrationRequest(r) {
				exchanges.Add(1)
				http.Error(w, "unexpected exchange", 400)
				return
			}
			sends.Add(1)
			id := r.Header.Get("X-Task1314-Provider")
			if !task1314IntegrationCredentials(r, id, next) {
				t.Error("wrong polled credentials")
				http.Error(w, "stale bearer", 400)
				return
			}
			task1314IntegrationReply(w, id, false)
		}))
		t.Cleanup(server.Close)
		models := make([]fantasy.LanguageModel, 0, 3)
		for _, id := range task1314IntegrationIDs {
			task1314IntegrationSeed(store, id, old)
			models = append(models, task1314IntegrationModel(t, counted, id, server, &bodies))
		}
		task1314IntegrationWriteAll(t, path, old)
		store.StartOAuthDiskSync()
		// No public poller join exists. This store-lifetime goroutine is contained by the child process boundary.
		task1314IntegrationWriteAll(t, path, next)
		require.Eventually(t, func() bool {
			for _, id := range task1314IntegrationIDs {
				pc, ok := store.Config().Providers.Get(id)
				if !ok || !reflect.DeepEqual(next, pc.OAuthToken) {
					return false
				}
			}
			return true
		}, 14*time.Second, 100*time.Millisecond)
		for _, model := range models {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			_, err := model.Generate(ctx, fantasy.Call{})
			cancel()
			require.NoError(t, err)
		}
		require.EqualValues(t, 3, sends.Load())
		require.Zero(t, counted.refreshes.Load())
		require.Zero(t, exchanges.Load())
		require.Equal(t, bodies.opened.Load(), bodies.closed.Load())
	})
	for _, id := range task1314IntegrationIDs {
		t.Run("concurrent4/"+id, func(t *testing.T) {
			// Revert-check: compare-refresh must publish disk credentials once and replay each of four rejected old snapshots exactly once.
			store, path := task1314LoadDiskStore(t)
			old, next := task1314IntegrationTokens()
			task1314IntegrationSeed(store, id, old)
			task1314WriteAgentDisk(t, path, id, old)
			counted := &task1314CountingStore{ConfigStore: store}
			var oldSends, newSends, exchanges atomic.Int32
			var bodies task1314IntegrationBodies
			arrivals, release := make(chan struct{}, 4), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !task1314IntegrationRequest(r) {
					exchanges.Add(1)
					http.Error(w, "unexpected exchange", 400)
					return
				}
				if task1314IntegrationCredentials(r, id, old) {
					oldSends.Add(1)
					select {
					case arrivals <- struct{}{}:
					case <-ctx.Done():
						return
					}
					select {
					case <-release:
					case <-ctx.Done():
						return
					}
					task1314IntegrationReply(w, id, true)
					return
				}
				if !task1314IntegrationCredentials(r, id, next) {
					t.Error("wrong retry credentials")
					http.Error(w, "bad bearer", 400)
					return
				}
				newSends.Add(1)
				task1314IntegrationReply(w, id, false)
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() { cancel(); unblock() })
			models := make([]fantasy.LanguageModel, 4)
			for i := range models {
				models[i] = task1314IntegrationModel(t, counted, id, server, &bodies)
			}
			before := store.Generation()
			results := make(chan error, 4)
			var wg sync.WaitGroup
			for _, model := range models {
				wg.Add(1)
				go func() { defer wg.Done(); _, err := model.Generate(ctx, fantasy.Call{}); results <- err }()
			}
			joined := make(chan struct{})
			go func() { wg.Wait(); close(joined) }()
			for range 4 {
				select {
				case <-arrivals:
				case <-ctx.Done():
					t.Error("old request barrier timed out")
					cancel()
					unblock()
				}
			}
			if ctx.Err() == nil {
				task1314WriteAgentDisk(t, path, id, next)
			}
			unblock()
			select {
			case <-joined:
			case <-time.After(10 * time.Second):
				t.Fatal("model goroutines did not join")
			}
			for range 4 {
				require.NoError(t, <-results)
			}
			require.EqualValues(t, 4, oldSends.Load())
			require.EqualValues(t, 4, newSends.Load())
			require.Zero(t, exchanges.Load())
			require.Equal(t, before+1, store.Generation())
			pc, ok := store.Config().Providers.Get(id)
			require.True(t, ok)
			require.Equal(t, next, pc.OAuthToken)
			require.Equal(t, bodies.opened.Load(), bodies.closed.Load())
		})
	}
	if !t.Failed() {
		require.NoError(t, os.WriteFile(filepath.Join(root, "complete"), []byte("ok"), 0o600))
	}
}
