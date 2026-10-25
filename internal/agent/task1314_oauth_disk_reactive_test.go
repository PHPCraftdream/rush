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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

func TestTask1314OAuthDiskReactiveSubprocess(t *testing.T) {
	// Revert-check: public config.Load plus production token sources must retain reactive disk authority across the real HTTP provider path.
	t.Parallel()
	root := t.TempDir()
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	command := platform.Command(ctx, executable, "-test.run=^TestTask1314OAuthDiskReactiveChild$", "-test.v", "-test.parallel=1")
	command.Env = append(os.Environ(), "TASK1314_DISK_CHILD="+root)
	output, err := command.CombinedOutput()
	t.Logf("Child output:\n%s", output)
	require.NoError(t, err)
	// Agent TestMain currently ignores m.Run's exit code, so require an explicit
	// completion file written only after every child assertion succeeds.
	_, err = os.Stat(filepath.Join(root, "complete"))
	require.NoError(t, err, "child did not complete all assertions: %s", output)
}

func task1314LoadDiskStore(t *testing.T) (*config.ConfigStore, string) {
	t.Helper()
	root := t.TempDir()
	for key, directory := range map[string]string{
		"RUSH_GLOBAL_CONFIG": "config", "XDG_CONFIG_HOME": "config",
		"RUSH_GLOBAL_DATA": "data", "XDG_DATA_HOME": "data",
		"RUSH_CACHE_DIR": "cache", "XDG_CACHE_HOME": "cache", "RUSH_SKILLS_DIR": "skills",
	} {
		path := filepath.Join(root, directory)
		require.NoError(t, os.MkdirAll(path, 0o700))
		t.Setenv(key, path)
	}
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	t.Setenv("CATWALK_URL", "embedded")
	config.ResetProviderCacheForTests()
	// Disabled synthetic entries avoid account catalog discovery during Load.
	providers := map[string]any{}
	for _, id := range []string{"openai-codex", "copilot", "hyper"} {
		providers[id] = map[string]any{
			"type": "openai-compat", "disable": true, "discover_models": false,
			"oauth": &oauth.Token{AccessToken: "fixture-load-access", RefreshToken: "fixture-load-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "fixture-account"},
		}
	}
	data, err := json.Marshal(map[string]any{"providers": providers})
	require.NoError(t, err)
	path := config.GlobalConfigData()
	require.NoError(t, os.WriteFile(path, data, 0o600))
	working := filepath.Join(root, "working")
	require.NoError(t, os.MkdirAll(working, 0o700))
	store, err := config.Load(working, filepath.Join(root, "workspace-data"), false)
	require.NoError(t, err)
	return store, path
}

func task1314WriteAgentDisk(t *testing.T, path, id string, token *oauth.Token) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"providers": map[string]any{id: map[string]any{"oauth": token}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func TestTask1314OAuthDiskReactiveChild(t *testing.T) {
	// Revert-check: preflightCodexDiskAuthority, refreshLegacyOAuthLeader and the provider single-retry guards enforce disk adoption, equal-token and logout behavior.
	root := os.Getenv("TASK1314_DISK_CHILD")
	if root == "" {
		t.Skip("subprocess helper")
	}
	// The child is a separate process: it inherits nothing from the
	// parent's package variables, so the gate must be disabled here too.
	// This is a legacy-JSON fixture: it rewrites rush.json at the literal
	// GlobalConfigData path and expects the reactive disk watcher to see it.
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)

	// This helper is serial; t.Setenv executes AFTER agent TestMain and never
	// changes the parent's environment or the machine's configuration files.
	for _, mode := range []string{"adopt", "second401", "equal", "logout"} {
		for _, id := range []string{"openai-codex", "copilot", "hyper"} {
			name := mode
			if mode == "logout" && id != "openai-codex" {
				name = "nil-disk-is-not-signout"
			}
			t.Run(id+"/"+name, func(t *testing.T) {
				// Revert-check: refreshLegacyOAuthLeader ignores a nil disk token (legacyDiskTokenAdoptable requires disk != nil); legacy providers have no sign-out proof (backlog #1319).
				store, path := task1314LoadDiskStore(t)
				old := &oauth.Token{AccessToken: "fixture-old", RefreshToken: "fixture-refresh-old", AccountID: "fixture-account", ExpiresAt: time.Now().Add(time.Hour).Unix()}
				next := &oauth.Token{AccessToken: "fixture-next", RefreshToken: "fixture-refresh-next", AccountID: "fixture-next-account", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()}
				pc := config.ProviderConfig{ID: id, Type: "openai-compat", APIKey: old.AccessToken, OAuthToken: old}
				store.SetProviderRuntimeConfig(id, pc)
				var sends, exchanges atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/oauth/token" || r.URL.Path == "/copilot_internal/v2/token" || strings.Contains(r.URL.Path, "token") && r.URL.Path != "/v1/chat/completions" {
						exchanges.Add(1)
						// Invalid success bytes produce a real exchange parse failure,
						// not Codex's rejected-exchange disk polling policy.
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, "{")
						return
					}
					n := sends.Add(1)
					if n == 1 {
						if r.Header.Get("Authorization") != "Bearer "+old.AccessToken {
							t.Error("first request did not use stale old bearer")
							http.Error(w, "wrong initial credentials", http.StatusBadRequest)
							return
						}
						disk := next
						switch mode {
						case "equal":
							disk = old
						case "logout":
							disk = nil
						}
						// Rotate only after the stale request is already on the wire.
						data, _ := json.Marshal(map[string]any{"providers": map[string]any{id: map[string]any{"oauth": disk}}})
						if err := os.WriteFile(path, data, 0o600); err != nil {
							http.Error(w, err.Error(), http.StatusInternalServerError)
							return
						}
					}
					if n == 1 || mode == "second401" || mode == "equal" || mode == "logout" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = io.WriteString(w, `{"error":{"message":"fixture-original-401","type":"authentication_error"}}`)
						return
					}
					if r.Header.Get("Authorization") != "Bearer "+next.AccessToken || id == "openai-codex" && r.Header.Get("chatgpt-account-id") != next.AccountID {
						http.Error(w, "wrong adopted credentials", http.StatusBadRequest)
						return
					}
					if id == "openai-codex" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
					}
				}))
				t.Cleanup(server.Close)
				endpoint, err := url.Parse(server.URL)
				require.NoError(t, err)
				base := server.Client().Transport.(*http.Transport).Clone()
				t.Cleanup(base.CloseIdleConnections)
				client := &http.Client{Timeout: 3 * time.Second, Transport: codexAdapterRoundTripper(func(r *http.Request) (*http.Response, error) {
					forwarded := r.Clone(r.Context())
					forwarded.URL.Scheme, forwarded.URL.Host = endpoint.Scheme, endpoint.Host
					forwarded.Host = endpoint.Host
					return base.RoundTrip(forwarded)
				})}
				counted := &task1314CountingStore{ConfigStore: store}
				resolve := func(*config.Config, config.ProviderConfig) (*http.Client, error) { return client, nil }
				coord := &coordinator{cfg: store}
				var provider fantasy.Provider
				if id == "openai-codex" {
					provider, err = newCodexTokenProvider(client, &codexTokenSource{store: counted, resolve: resolve})
				} else {
					wrapped := coord.withOAuthBearerClient(pc, client)
					source := wrapped.Transport.(*oauthBearerTransport).source.(*oauthTokenSource)
					source.store, source.resolve = counted, resolve
					provider, err = coord.buildOpenaiCompatProvider(store.Config(), server.URL+"/v1", old.AccessToken, nil, nil, id, false, wrapped)
				}
				require.NoError(t, err)
				model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
				require.NoError(t, err)
				task1314WriteAgentDisk(t, path, id, old)
				before := store.Generation()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				_, err = model.Generate(ctx, fantasy.Call{})
				require.EqualValues(t, 1, counted.refreshes.Load())
				if mode == "adopt" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "fixture-original-401")
				}
				if mode == "adopt" || mode == "second401" {
					require.EqualValues(t, 2, sends.Load())
					require.Zero(t, exchanges.Load())
					require.Equal(t, before+1, store.Generation())
					live, ok := store.Config().Providers.Get(id)
					require.True(t, ok)
					require.Equal(t, next, live.OAuthToken)
				} else {
					require.EqualValues(t, 1, sends.Load())
					if mode == "equal" || id != "openai-codex" {
						require.EqualValues(t, 1, exchanges.Load(), "real failing token endpoint was reached")
						live, ok := store.Config().Providers.Get(id)
						require.True(t, ok)
						require.Equal(t, old, live.OAuthToken)
						require.Equal(t, before, store.Generation())
					} else {
						require.Zero(t, exchanges.Load())
						live, ok := store.Config().Providers.Get(id)
						require.True(t, ok)
						require.Nil(t, live.OAuthToken, "proven disk logout must clear live credentials")
					}
				}
			})
		}
	}
	if !t.Failed() {
		require.NoError(t, os.WriteFile(filepath.Join(root, "complete"), []byte("ok"), 0o600))
	}
}
