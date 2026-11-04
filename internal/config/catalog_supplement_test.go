package config

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/stretchr/testify/require"
)

func TestSupplementalCatalogModelLoadsForConfiguredAnthropic(t *testing.T) {
	isolateConfigEnvironment(t)
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.Providers.Set("anthropic", ProviderConfig{})
	cfg.setDefaults(t.TempDir(), "")
	predecessor := catwalk.Model{
		ID:                 "claude-haiku-4-5-20251001",
		CostPer1MIn:        0.8,
		CostPer1MOut:       4,
		CostPer1MInCached:  0.08,
		CostPer1MOutCached: 0.4,
	}
	provider := catwalk.Provider{
		ID:     catwalk.InferenceProviderAnthropic,
		Name:   "Anthropic",
		APIKey: "$ANTHROPIC_API_KEY",
		Models: []catwalk.Model{{ID: "claude-sonnet-4-6"}, predecessor},
	}
	e := env.NewFromMap(map[string]string{"ANTHROPIC_API_KEY": "test-key"})
	require.NoError(t, cfg.configureProviders(context.Background(), testStore(cfg), e, NewShellVariableResolver(e), []catwalk.Provider{provider}))

	loaded, ok := cfg.Providers.Get("anthropic")
	require.True(t, ok)
	model, ok := findCatalogModel(loaded.Models, "claude-haiku-5-5")
	require.True(t, ok)
	require.Equal(t, "Claude Haiku 5.5", model.Name)
	require.EqualValues(t, 1_000_000, model.ContextWindow)
	require.Contains(t, model.ReasoningLevels, "xhigh")
	require.Equal(t, predecessor.CostPer1MIn, model.CostPer1MIn)
	require.Equal(t, predecessor.CostPer1MOut, model.CostPer1MOut)
	require.Equal(t, predecessor.CostPer1MInCached, model.CostPer1MInCached)
	require.Equal(t, predecessor.CostPer1MOutCached, model.CostPer1MOutCached)
}

func TestSupplementalCatalogDoesNotOverrideExistingID(t *testing.T) {
	existing := catwalk.Model{ID: "claude-haiku-5-5", Name: "Catwalk's model", ContextWindow: 12345, CostPer1MIn: 2}
	providers := mergeSupplementalCatalogModels([]catwalk.Provider{{
		ID:     catwalk.InferenceProviderAnthropic,
		Models: []catwalk.Model{existing},
	}})
	require.Len(t, providers[0].Models, 1)
	require.Equal(t, existing, providers[0].Models[0])
}

func isolateConfigEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
}

func findCatalogModel(models []catwalk.Model, id string) (catwalk.Model, bool) {
	for _, model := range models {
		if model.ID == id {
			return model, true
		}
	}
	return catwalk.Model{}, false
}
