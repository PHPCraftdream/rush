package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/stretchr/testify/require"
)

// TestConfigureProviders_ZeroDumpDoesNotShadowCatalog: `rush providers update
// zai` writes the raw /models answer into rush.json, which carries no context
// window at all. Such a zero is unknown, so the catalog's value for the same
// model must show through, field by field; a positive value the user wrote
// still wins over the catalog.
//
// Revert-check: drop the ContextWindow fill, the DefaultMaxTokens fill, or
// make either fill unconditional (ignoring the user's value) in
// configureProviders and the matching row goes red.
func TestConfigureProviders_ZeroDumpDoesNotShadowCatalog(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	zaiModels := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer zaiModels.Close()

	origDocs := discoverZAIEffortDocs
	discoverZAIEffortDocs = func(context.Context) ([]catwalk.Model, error) { return nil, nil }
	t.Cleanup(func() { discoverZAIEffortDocs = origDocs })
	origAvailable := cliprovider.AvailableFunc
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return nil }
	t.Cleanup(func() { cliprovider.AvailableFunc = origAvailable })

	catalog := zaiKnownProvider()
	catalog.Models = []catwalk.Model{
		{ID: "glm-4.7", ContextWindow: 204_800, DefaultMaxTokens: 98_000},
		{ID: "glm-4.6", ContextWindow: 204_800, DefaultMaxTokens: 102_400},
		{ID: "glm-4.5", ContextWindow: 131_072, DefaultMaxTokens: 49_152},
	}

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Providers.Set("zai", ProviderConfig{
		ID:      "zai",
		BaseURL: zaiModels.URL,
		APIKey:  "zai-test-key",
		Models: []catwalk.Model{
			{ID: "glm-4.7", Name: "glm-4.7"},
			{ID: "glm-4.6", Name: "glm-4.6", ContextWindow: 150_000},
			{ID: "glm-4.5", Name: "glm-4.5", DefaultMaxTokens: 4096},
		},
	})
	envMap := env.NewFromMap(map[string]string{"ZAI_API_KEY": "zai-test-key"})
	require.NoError(t, cfg.configureProviders(
		context.Background(), testStore(cfg), envMap, NewShellVariableResolver(envMap),
		[]catwalk.Provider{catalog},
	))

	pc, ok := cfg.Providers.Get("zai")
	require.True(t, ok)
	byID := map[string]catwalk.Model{}
	for _, m := range pc.Models {
		byID[m.ID] = m
	}
	require.EqualValues(t, 204_800, byID["glm-4.7"].ContextWindow, "a zero dump takes the catalog window")
	require.EqualValues(t, 98_000, byID["glm-4.7"].DefaultMaxTokens, "a zero dump takes the catalog output budget")
	require.EqualValues(t, 150_000, byID["glm-4.6"].ContextWindow, "the user's window wins over the catalog")
	require.EqualValues(t, 102_400, byID["glm-4.6"].DefaultMaxTokens, "the unset budget still takes the catalog value")
	require.EqualValues(t, 131_072, byID["glm-4.5"].ContextWindow, "the unset window still takes the catalog value")
	require.EqualValues(t, 4096, byID["glm-4.5"].DefaultMaxTokens, "the user's budget wins over the catalog")
}
