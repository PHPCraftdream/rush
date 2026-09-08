package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildProviderZAIExtraBodySnapshotIsolation(t *testing.T) {
	extraBody := map[string]any{
		"keep":    "caller-value",
		"nested":  map[string]any{"enabled": true},
		"options": []any{"one", float64(2)},
	}
	store := config.NewLibraryStore(&config.Config{
		Options: &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"zai": {
				ID:        string(catwalk.InferenceProviderZAI),
				Type:      openaicompat.Name,
				APIKey:    "configured-key",
				ExtraBody: extraBody,
			},
		}),
	}, t.TempDir())
	coord := &coordinator{cfg: store}
	snapshot, _ := store.Snapshot()
	providerCfg, ok := snapshot.Providers.Get("zai")
	require.True(t, ok)
	wantExtraBody, err := json.Marshal(providerCfg.ExtraBody)
	require.NoError(t, err)

	const builds = 64
	start := make(chan struct{})
	providers := make(chan fantasy.Provider, builds)
	errs := make(chan error, builds)
	var wg sync.WaitGroup
	for range builds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			provider, err := coord.buildProvider(providerCfg, config.SelectedModel{}, false)
			if err != nil {
				errs <- err
				return
			}
			providers <- provider
		}()
	}
	close(start)
	wg.Wait()
	close(providers)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	seen := make(map[uintptr]struct{}, builds)
	for provider := range providers {
		require.NotNil(t, provider)
		identity := reflect.ValueOf(provider)
		require.Equal(t, reflect.Pointer, identity.Kind())
		_, duplicate := seen[identity.Pointer()]
		assert.False(t, duplicate, "concurrent builds must create distinct provider instances")
		seen[identity.Pointer()] = struct{}{}
	}
	require.Len(t, seen, builds)

	gotExtraBody, err := json.Marshal(providerCfg.ExtraBody)
	require.NoError(t, err)
	assert.Equal(t, wantExtraBody, gotExtraBody)
	after, _ := store.Snapshot()
	afterProviderCfg, ok := after.Providers.Get("zai")
	require.True(t, ok)
	afterExtraBody, err := json.Marshal(afterProviderCfg.ExtraBody)
	require.NoError(t, err)
	assert.Equal(t, wantExtraBody, afterExtraBody)
	assert.NotContains(t, string(afterExtraBody), "tool_stream")
}

func TestBuildAnthropicProviderPreservesEnvironmentAndAuthSelection(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sentinel-process-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sentinel-process-token")

	var requestHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHeaders = r.Header.Clone()
		http.Error(w, "probe", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	store := config.NewLibraryStore(&config.Config{Options: &config.Options{}}, t.TempDir())
	coord := &coordinator{cfg: store}
	tests := []struct {
		name        string
		providerID  string
		apiKey      string
		headers     map[string]string
		authorize   string
		wantAPIKey  string
		wantAuthKey string
	}{
		{
			name:        "bearer token overrides explicit auth headers",
			providerID:  "anthropic",
			apiKey:      "Bearer configured-bearer",
			headers:     map[string]string{"authorization": "operator-lower", "Authorization": "operator-canonical", "x-api-key": "operator-lower-key", "X-Api-Key": "operator-canonical-key"},
			authorize:   "Bearer configured-bearer",
			wantAuthKey: "",
		},
		{
			name:        "minimax token overrides explicit auth headers",
			providerID:  string(catwalk.InferenceProviderMiniMax),
			apiKey:      "configured-minimax",
			headers:     map[string]string{"AUTHORIZATION": "operator-upper", "Authorization": "operator-canonical", "X-API-KEY": "operator-upper-key", "x-api-key": "operator-canonical-key"},
			authorize:   "Bearer configured-minimax",
			wantAuthKey: "",
		},
		{
			name:        "api key preserves explicit authorization",
			providerID:  "anthropic",
			apiKey:      "configured-api-key",
			headers:     map[string]string{"authorization": "operator-lower", "Authorization": "operator-canonical", "X-API-KEY": "operator-upper-key", "x-api-key": "operator-canonical-key"},
			authorize:   "operator-canonical",
			wantAuthKey: "configured-api-key",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requestHeaders = nil
			wantHeaders := maps.Clone(tc.headers)
			provider, err := coord.buildAnthropicProvider(server.URL, tc.apiKey, tc.headers, tc.providerID)
			require.NoError(t, err)
			assert.Equal(t, wantHeaders, tc.headers)
			model, err := provider.LanguageModel(context.Background(), "probe-model")
			require.NoError(t, err)
			_, err = model.Generate(context.Background(), fantasy.Call{})
			require.Error(t, err)
			require.NotNil(t, requestHeaders)
			assert.Equal(t, tc.authorize, requestHeaders.Get("Authorization"))
			assert.Equal(t, tc.wantAuthKey, requestHeaders.Get("X-Api-Key"))
			if tc.authorize != "" {
				require.Len(t, requestHeaders.Values("Authorization"), 1)
			} else {
				assert.LessOrEqual(t, len(requestHeaders.Values("Authorization")), 1)
			}
			if tc.wantAuthKey != "" {
				require.Len(t, requestHeaders.Values("X-Api-Key"), 1)
			} else {
				assert.LessOrEqual(t, len(requestHeaders.Values("X-Api-Key")), 1)
			}
			for _, value := range requestHeaders.Values("Authorization") {
				assert.NotContains(t, value, "sentinel-")
			}
			for _, value := range requestHeaders.Values("X-Api-Key") {
				assert.NotContains(t, value, "sentinel-")
			}
			for _, values := range requestHeaders {
				for _, value := range values {
					assert.NotContains(t, value, "sentinel-")
				}
			}
			assert.Equal(t, "sentinel-process-key", os.Getenv("ANTHROPIC_API_KEY"))
			assert.Equal(t, "sentinel-process-token", os.Getenv("ANTHROPIC_AUTH_TOKEN"))
		})
	}
}

func TestBuildAnthropicProviderConcurrentHeterogeneousBuildsPreserveEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sentinel-process-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sentinel-process-token")
	store := config.NewLibraryStore(&config.Config{Options: &config.Options{}}, t.TempDir())
	coord := &coordinator{cfg: store}
	cases := []struct {
		providerID string
		apiKey     string
	}{
		{providerID: "anthropic", apiKey: "Bearer bearer-one"},
		{providerID: string(catwalk.InferenceProviderMiniMax), apiKey: "minimax-one"},
		{providerID: "anthropic", apiKey: "api-key-one"},
	}

	const buildsPerCase = 32
	start := make(chan struct{})
	errs := make(chan error, len(cases)*buildsPerCase)
	var wg sync.WaitGroup
	for _, tc := range cases {
		for range buildsPerCase {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := coord.buildAnthropicProvider("", tc.apiKey, map[string]string{"aUtHoRiZaTiOn": "operator", "X-API-KEY": "operator-key"}, tc.providerID)
				if err != nil {
					errs <- err
				}
				if got := os.Getenv("ANTHROPIC_API_KEY"); got != "sentinel-process-key" {
					errs <- errors.New("ANTHROPIC_API_KEY changed")
				}
				if got := os.Getenv("ANTHROPIC_AUTH_TOKEN"); got != "sentinel-process-token" {
					errs <- errors.New("ANTHROPIC_AUTH_TOKEN changed")
				}
			}()
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	_, err := coord.buildProvider(config.ProviderConfig{ID: "unsupported", Type: "unsupported"}, config.SelectedModel{}, false)
	require.Error(t, err)
	assert.Equal(t, "sentinel-process-key", os.Getenv("ANTHROPIC_API_KEY"))
	assert.Equal(t, "sentinel-process-token", os.Getenv("ANTHROPIC_AUTH_TOKEN"))
}
