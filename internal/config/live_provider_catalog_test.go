package config

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/stretchr/testify/require"
)

func TestStepFunLiveRosterReplacesRetiredSeedAndCachesEfforts(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"step-new","reasoning_effort_support_list":["low","high"]}]}`))
	}))
	defer server.Close()

	catalog := []catwalk.Provider{{ID: "stepfun", Name: "StepFun", Type: catwalk.TypeOpenAICompat,
		APIEndpoint: server.URL + "/v1", APIKey: "key", Models: []catwalk.Model{{ID: "retired-step"}}}}
	load := func() ProviderConfig {
		cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
		cfg.setDefaults(t.TempDir(), "")
		require.NoError(t, cfg.configureProviders(t.Context(), testStore(cfg), env.NewFromMap(nil),
			NewShellVariableResolver(env.NewFromMap(nil)), catalog))
		provider, ok := cfg.Providers.Get("stepfun")
		require.True(t, ok)
		return provider
	}
	provider := load()
	require.Len(t, provider.Models, 1)
	require.Equal(t, "step-new", provider.Models[0].ID)
	require.Equal(t, []string{"low", "high"}, provider.LiveEfforts["step-new"].Levels)
	load()
	require.EqualValues(t, 1, calls.Load(), "fresh global cache must avoid duplicate model requests")
}

func TestZAILiveRosterAddsModelsWithoutDiscardingKnownModels(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	originalDocs := discoverZAIEffortDocs
	discoverZAIEffortDocs = func(context.Context) ([]catwalk.Model, error) { return nil, fmt.Errorf("docs unavailable") }
	t.Cleanup(func() { discoverZAIEffortDocs = originalDocs })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-new"},{"id":"glm-5.3","reasoning_effort_support_list":["low","high","max"]}]}`))
	}))
	defer server.Close()

	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.setDefaults(t.TempDir(), "")
	require.NoError(t, cfg.configureProviders(context.Background(), testStore(cfg), env.NewFromMap(nil),
		NewShellVariableResolver(env.NewFromMap(nil)), []catwalk.Provider{{
			ID: catwalk.InferenceProviderZAI, Name: "Z.AI", Type: catwalk.TypeOpenAICompat,
			APIEndpoint: server.URL, APIKey: "key", Models: []catwalk.Model{{ID: "glm-5.2"}},
		}}))
	provider, ok := cfg.Providers.Get("zai")
	require.True(t, ok)
	ids := make(map[string]bool, len(provider.Models))
	for _, model := range provider.Models {
		ids[model.ID] = true
	}
	require.True(t, ids["glm-5.2"])
	require.True(t, ids["glm-5.3"])
	require.True(t, ids["glm-new"])
	require.Equal(t, []string{"low", "high", "max"}, provider.LiveEfforts["glm-5.3"].Levels)
	require.Empty(t, provider.LiveEfforts["glm-new"].Levels)
}

func TestZAIDocumentedEffortsApplyWhenModelsAPIHasNoCapabilities(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	originalDocs := discoverZAIEffortDocs
	discoverZAIEffortDocs = func(context.Context) ([]catwalk.Model, error) {
		return []catwalk.Model{{ID: "glm-5.3", ReasoningLevels: []string{"low", "high", "max"}}}, nil
	}
	t.Cleanup(func() { discoverZAIEffortDocs = originalDocs })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3"},{"id":"glm-future"}]}`))
	}))
	defer server.Close()

	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.setDefaults(t.TempDir(), "")
	require.NoError(t, cfg.configureProviders(t.Context(), testStore(cfg), env.NewFromMap(nil),
		NewShellVariableResolver(env.NewFromMap(nil)), []catwalk.Provider{{
			ID: catwalk.InferenceProviderZAI, Name: "Z.AI", Type: catwalk.TypeOpenAICompat,
			APIEndpoint: server.URL, APIKey: "key",
		}}))
	provider, ok := cfg.Providers.Get("zai")
	require.True(t, ok)
	require.Equal(t, []string{"low", "high", "max"}, provider.LiveEfforts["glm-5.3"].Levels)
	require.Empty(t, provider.LiveEfforts["glm-future"].Levels, "undocumented new models must not get guessed tiers")
}
