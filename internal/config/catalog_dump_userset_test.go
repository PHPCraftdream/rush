package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/discover"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

// codexLiveRow mirrors a raw row of the codex account catalog as
// DiscoverCodexModels serves it: the positive numbers a
// `rush providers update openai-codex` dump writes into rush.json, before
// the facts overlay lifts them to the documented values.
func codexLiveRow() catwalk.Model {
	return catwalk.Model{
		ID:               "gpt-6.1-sol",
		Name:             "GPT-6.1 Sol",
		ContextWindow:    272_000, // the account catalog's raw number; the facts table documents 1,050,000
		DefaultMaxTokens: 128_000,
	}
}

// setupCodexCatalogDumpTest isolates the model-catalog cache and local-CLI
// detection, then seeds a fresh codex account-catalog cache entry with the
// raw row so configureProviders serves it without touching the network (the
// golden test's pattern).
func setupCodexCatalogDumpTest(t *testing.T) catwalk.Model {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	origAvailable := cliprovider.AvailableFunc
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return nil }
	t.Cleanup(func() { cliprovider.AvailableFunc = origAvailable })

	live := codexLiveRow()
	seedModelCatalog(t, "openai-codex", discover.CodexBaseURL, "chatgpt-account", []catwalk.Model{live})
	return live
}

// runCodexDumpScenario loads the given rush.json provider state through
// configureProviders with the given known-provider list and returns the
// resulting openai-codex models by ID.
func runCodexDumpScenario(t *testing.T, cfg *Config, known []catwalk.Provider) map[string]catwalk.Model {
	t.Helper()
	envMap := env.NewFromMap(map[string]string{})
	require.NoError(t, cfg.configureProviders(
		context.Background(), testStore(cfg), envMap, NewShellVariableResolver(envMap), known,
	))
	pc, ok := cfg.Providers.Get("openai-codex")
	require.True(t, ok)
	byID := make(map[string]catwalk.Model, len(pc.Models))
	for _, m := range pc.Models {
		byID[m.ID] = m
	}
	return byID
}

// TestConfigureProviders_CatalogDumpDoesNotCountAsUserSet reproduces the
// production wiring: the known-provider entry for openai-codex carries no
// models (codexProvider), the account catalog arrives from
// cachedProviderModels inside configureProviders, and rush.json holds a
// `rush providers update` dump whose numbers equal the live row's raw
// values. That dump is not a user decision: it must not stay user-set, and
// the facts overlay must still lift the values.
//
// Revert-check: drop the forgetDumpedUserSet call in the openai-codex
// branch and the dumped window stays at the account catalog's raw number
// instead of the facts value.
func TestConfigureProviders_CatalogDumpDoesNotCountAsUserSet(t *testing.T) {
	live := setupCodexCatalogDumpTest(t)

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Providers.Set("openai-codex", ProviderConfig{
		ID:         "openai-codex",
		OAuthToken: &oauth.Token{AccessToken: "rush-access-token", AccountID: "chatgpt-account"},
		// The dump exactly as `rush providers update` wrote it: the live
		// row's raw numbers, positive and untouched.
		Models: []catwalk.Model{{
			ID:               live.ID,
			Name:             live.Name,
			ContextWindow:    live.ContextWindow,
			DefaultMaxTokens: live.DefaultMaxTokens,
		}},
	})

	byID := runCodexDumpScenario(t, cfg, []catwalk.Provider{codexProvider()})

	factsWindow, factsMaxTokens := discover.LookupModelFacts("openai-codex", live.ID)
	require.NotZero(t, factsWindow, "the facts table must have a row for the dumped model")
	require.EqualValues(t, factsWindow, byID[live.ID].ContextWindow,
		"a dump of the account catalog's raw window must be lifted to the documented facts value")
	require.EqualValues(t, factsMaxTokens, byID[live.ID].DefaultMaxTokens,
		"a dump of the account catalog's raw output budget must end at the documented facts value, not stay pinned as user-set")
}

// TestConfigureProviders_UserValueDifferentFromCatalogWins: a positive value
// that differs from the catalog's raw number is the user's own decision and
// stays exactly as written, over both the catalog and the facts floor.
//
// Revert-check: pins the pre-existing behaviour so the dump fix cannot grow
// into overriding genuine user values.
func TestConfigureProviders_UserValueDifferentFromCatalogWins(t *testing.T) {
	live := setupCodexCatalogDumpTest(t)

	userWindow := live.ContextWindow + 1000
	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Providers.Set("openai-codex", ProviderConfig{
		ID:         "openai-codex",
		OAuthToken: &oauth.Token{AccessToken: "rush-access-token", AccountID: "chatgpt-account"},
		// The window differs from the catalog (a decision); the output
		// budget equals it (a dump).
		Models: []catwalk.Model{{
			ID:               live.ID,
			Name:             live.Name,
			ContextWindow:    userWindow,
			DefaultMaxTokens: live.DefaultMaxTokens,
		}},
	})

	// A known-provider entry that also carries the model, as the catwalk
	// list does for catalog providers: the same dump check runs there.
	catalog := codexProvider()
	catalog.Models = []catwalk.Model{live}

	byID := runCodexDumpScenario(t, cfg, []catwalk.Provider{catalog})

	factsWindow, factsMaxTokens := discover.LookupModelFacts("openai-codex", live.ID)
	require.NotEqual(t, factsWindow, userWindow, "the test must pick a window the facts table would change")
	require.EqualValues(t, userWindow, byID[live.ID].ContextWindow,
		"the user's window differs from the catalog and wins over the facts floor")
	require.EqualValues(t, factsMaxTokens, byID[live.ID].DefaultMaxTokens,
		"the output budget equals the catalog's raw value (a dump) and ends at the documented facts value")
}

// TestConfigureProviders_CustomProviderValueStaysUserSet: a custom provider
// has no catalog entry, so its positive values are user-set even though a
// facts row matches the provider; the overlay must leave them alone.
//
// Revert-check: invert the no-catalog branch (treat a missing catalog entry
// as not user-set) and the facts floor overwrites the custom values.
func TestConfigureProviders_CustomProviderValueStaysUserSet(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	origAvailable := cliprovider.AvailableFunc
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return nil }
	t.Cleanup(func() { cliprovider.AvailableFunc = origAvailable })

	customModels := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer customModels.Close()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	// Provider "zai" is NOT in the known-provider list below, so it is a
	// custom provider with no catalog entry -- but the zai facts rows match
	// its model, so a wrong inversion of the dump check would be caught by
	// the floor overwriting these values.
	cfg.Providers.Set("zai", ProviderConfig{
		ID:      "zai",
		BaseURL: customModels.URL,
		Type:    catwalk.TypeOpenAI,
		Models: []catwalk.Model{
			{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", ContextWindow: 500_000, DefaultMaxTokens: 4096},
		},
	})
	envMap := env.NewFromMap(map[string]string{})
	require.NoError(t, cfg.configureProviders(
		context.Background(), testStore(cfg), envMap, NewShellVariableResolver(envMap),
		[]catwalk.Provider{},
	))

	pc, ok := cfg.Providers.Get("zai")
	require.True(t, ok)
	byID := map[string]catwalk.Model{}
	for _, m := range pc.Models {
		byID[m.ID] = m
	}
	require.EqualValues(t, 500_000, byID["glm-5.3-flash"].ContextWindow,
		"a custom provider's window has no catalog entry and stays user-set")
	require.EqualValues(t, 4096, byID["glm-5.3-flash"].DefaultMaxTokens,
		"a custom provider's output budget has no catalog entry and stays user-set")
}

// forgetDumpedUserSet is the single place a dump is told from a decision: the
// flags clear per field on equality with the catalog's raw number, an entry
// with nothing left is dropped, and rows the catalog does not know keep theirs.
// (The facts overlay only fills a ZERO output budget, so the output-budget flag
// is not observable end to end except through the window clamp -- this pins it
// directly.)
//
// Revert check: dropping either field's clear, or the entry deletion, fails a row.
func TestForgetDumpedUserSet(t *testing.T) {
	t.Parallel()

	catalog := []catwalk.Model{{ID: "m", ContextWindow: 100, DefaultMaxTokens: 10}}
	for _, tc := range []struct {
		name   string
		config catwalk.Model
		want   map[string]discover.UserModelFacts
	}{
		{"both equal: the entry goes away", catwalk.Model{ID: "m", ContextWindow: 100, DefaultMaxTokens: 10}, map[string]discover.UserModelFacts{}},
		{"window differs: only the window stays", catwalk.Model{ID: "m", ContextWindow: 200, DefaultMaxTokens: 10}, map[string]discover.UserModelFacts{"m": {ContextWindow: true}}},
		{"output budget differs: only the budget stays", catwalk.Model{ID: "m", ContextWindow: 100, DefaultMaxTokens: 20}, map[string]discover.UserModelFacts{"m": {DefaultMaxTokens: true}}},
		{"not in the catalog: untouched", catwalk.Model{ID: "other", ContextWindow: 100, DefaultMaxTokens: 10}, map[string]discover.UserModelFacts{"other": {ContextWindow: true, DefaultMaxTokens: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			userSet := map[string]map[string]discover.UserModelFacts{
				"p": {tc.config.ID: {ContextWindow: tc.config.ContextWindow > 0, DefaultMaxTokens: tc.config.DefaultMaxTokens > 0}},
			}
			forgetDumpedUserSet(userSet, "p", []catwalk.Model{tc.config}, catalog)
			require.Equal(t, tc.want, userSet["p"])
		})
	}
}

// A provider that ships its catalog with the known-provider list (the catwalk
// path) is handled at the same place as a custom one is not: a config row that
// equals the shipped catalog's raw window is a dump and the facts floor lifts it.
//
// Revert check: dropping the post-loop forgetDumpedUserSet call over
// knownProviders leaves the dumped window user-set at the catalog's raw value.
func TestConfigureProviders_KnownProviderCatalogDumpDoesNotCountAsUserSet(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	origAvailable := cliprovider.AvailableFunc
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return nil }
	t.Cleanup(func() { cliprovider.AvailableFunc = origAvailable })

	const modelID = "glm-5.3-flash"
	rawWindow := int64(200_000)
	catalog := catwalk.Provider{
		ID: "zai", Name: "Z.AI", Type: catwalk.TypeOpenAI, APIEndpoint: "http://127.0.0.1:1",
		Models: []catwalk.Model{{ID: modelID, Name: "GLM-5.3-Flash", ContextWindow: rawWindow, DefaultMaxTokens: 4096}},
	}
	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	noDiscovery := false
	cfg.Providers.Set("zai", ProviderConfig{
		ID: "zai", APIKey: "k", AutoDiscoverModels: &noDiscovery,
		// The dump exactly as `rush providers update` wrote the catalog row.
		Models: []catwalk.Model{{ID: modelID, Name: "GLM-5.3-Flash", ContextWindow: rawWindow, DefaultMaxTokens: 4096}},
	})
	envMap := env.NewFromMap(map[string]string{})
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
	factsWindow, _ := discover.LookupModelFacts("zai", modelID)
	require.NotZero(t, factsWindow)
	require.NotEqual(t, factsWindow, rawWindow, "the raw catalog window must differ from the documented one")
	require.EqualValues(t, factsWindow, byID[modelID].ContextWindow,
		"a dump equal to the shipped catalog's raw window must be lifted to the documented facts value")
}
