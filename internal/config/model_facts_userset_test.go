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

// TestConfigureProviders_UserSetsOnlyOneFieldTheOtherStaysUnknown: a config
// entry that pins just one of ContextWindow/DefaultMaxTokens leaves the other
// at 0 = unknown, so the documented facts still fill it, while the pinned
// field is the user's and is never touched.
//
// Revert-check: mark the window as user-set whenever the entry has any value
// (userSet ContextWindow: true) and glm-5.3 keeps window 0; likewise for
// DefaultMaxTokens and glm-5.3-flash keeps max 0.
func TestConfigureProviders_UserSetsOnlyOneFieldTheOtherStaysUnknown(t *testing.T) {
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

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Providers.Set("zai", ProviderConfig{
		ID:      "zai",
		BaseURL: zaiModels.URL,
		APIKey:  "zai-test-key",
		Models: []catwalk.Model{
			{ID: "glm-5.3", Name: "GLM-5.3", DefaultMaxTokens: 4096},
			{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", ContextWindow: 500_000},
		},
	})
	envMap := env.NewFromMap(map[string]string{"ZAI_API_KEY": "zai-test-key"})
	require.NoError(t, cfg.configureProviders(
		context.Background(), testStore(cfg), envMap, NewShellVariableResolver(envMap),
		[]catwalk.Provider{zaiKnownProvider()},
	))

	pc, ok := cfg.Providers.Get("zai")
	require.True(t, ok)
	byID := map[string]catwalk.Model{}
	for _, m := range pc.Models {
		byID[m.ID] = m
	}
	require.EqualValues(t, 1_000_000, byID["glm-5.3"].ContextWindow, "an unpinned window is filled from the facts table")
	require.EqualValues(t, 4096, byID["glm-5.3"].DefaultMaxTokens, "the user's output budget wins")
	require.EqualValues(t, 500_000, byID["glm-5.3-flash"].ContextWindow, "the user's window wins, even below the documented one")
	require.EqualValues(t, 131_072, byID["glm-5.3-flash"].DefaultMaxTokens, "an unpinned output budget is filled from the facts table")
}
