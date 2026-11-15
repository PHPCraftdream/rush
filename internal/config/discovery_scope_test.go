package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config/oauth"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/PHPCraftdream/rush/internal/kit/sys/env"
	"github.com/stretchr/testify/require"
)

func TestConfigureProvidersWithScope_DiscoveryScoping(t *testing.T) {
	type testCase struct {
		name          string
		scope         map[string]bool
		codexScoped   bool
		wantCodexHits int32
		wantZAIHits   int32
		wantDocsCalls int32
	}
	cases := []testCase{
		{name: "scoped to zai", scope: map[string]bool{"zai": true}, codexScoped: false, wantCodexHits: 0, wantZAIHits: 1, wantDocsCalls: 1},
		{name: "no scope", scope: nil, wantCodexHits: 1, wantZAIHits: 1, wantDocsCalls: 1},
		{name: "scoped to codex", scope: map[string]bool{"openai-codex": true}, wantCodexHits: 1, wantZAIHits: 0, wantDocsCalls: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigEnvironment(t)
			t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "")
			uniqueSuffix := tc.name

			var codexHits, zaiHits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("chatgpt-account-id") != "" {
					codexHits.Add(1)
				} else {
					zaiHits.Add(1)
				}
				_, _ = w.Write([]byte(`{"data":[{"id":"discovered-model"}]}`))
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			recordFactoryArgs(t, &routingTransport{target: target})

			var docsCalls atomic.Int32
			originalDocs := discoverZAIEffortDocs
			discoverZAIEffortDocs = func(context.Context) ([]catwalk.Model, error) {
				docsCalls.Add(1)
				return []catwalk.Model{{ID: "glm-5", ReasoningLevels: []string{"low", "high"}}}, nil
			}
			t.Cleanup(func() { discoverZAIEffortDocs = originalDocs })

			cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
			cfg.setDefaults(t.TempDir(), "")
			cfg.Providers.Set(string(catwalk.InferenceProviderZAI), ProviderConfig{
				ID:      string(catwalk.InferenceProviderZAI),
				BaseURL: server.URL,
				APIKey:  "zai-key-" + uniqueSuffix,
			})
			cfg.Providers.Set("openai-codex", ProviderConfig{
				ID:         "openai-codex",
				OAuthToken: &oauth.Token{AccessToken: "tok-" + uniqueSuffix, AccountID: "acc-" + uniqueSuffix},
			})
			known := []catwalk.Provider{zaiKnownProvider(), codexProvider()}
			environ := env.NewFromMap(nil)
			resolver := NewShellVariableResolver(environ)

			require.NoError(t, configureProvidersWithScope(context.Background(), cfg, environ, resolver, known, testStore(cfg).removeConfigFieldBestEffort, nil, tc.scope))

			require.GreaterOrEqual(t, codexHits.Load(), tc.wantCodexHits)
			require.GreaterOrEqual(t, zaiHits.Load(), tc.wantZAIHits)
			require.GreaterOrEqual(t, docsCalls.Load(), tc.wantDocsCalls)
			if tc.wantCodexHits == 0 {
				require.Equal(t, int32(0), codexHits.Load(), "scoped-out provider must receive zero discovery requests")
			}
			if tc.wantZAIHits == 0 {
				require.Equal(t, int32(0), zaiHits.Load(), "scoped-out provider must receive zero discovery requests")
			}
			if tc.wantDocsCalls == 0 {
				require.Equal(t, int32(0), docsCalls.Load())
			}

			pc, ok := cfg.Providers.Get(string(catwalk.InferenceProviderZAI))
			require.True(t, ok, "zai provider must stay registered")
			if tc.wantDocsCalls > 0 {
				_, found := findCatalogModel(pc.Models, "discovered-model")
				require.True(t, found, "zai provider must merge the discovered model")
			}
			codex, ok := cfg.Providers.Get("openai-codex")
			require.True(t, ok, "codex provider must stay registered even when its discovery is skipped")
			if tc.wantCodexHits == 0 {
				require.Empty(t, codex.Models, "skipped codex discovery must leave its models empty")
			}
		})
	}
}
