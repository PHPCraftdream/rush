package models

import (
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelsUseCmdCommandPath guards the literal in commandDiscoveryScope's
// switch: if the real command tree's path ever changes, scoping would
// silently stop applying to `rush models use`.
func TestModelsUseCmdCommandPath(t *testing.T) {
	root := &cobra.Command{Use: "rush"}
	root.AddCommand(NewModelsCmd())
	use, _, err := root.Find([]string{"models", "use"})
	require.NoError(t, err)
	require.Equal(t, "rush models use", use.CommandPath())
}

// TestModelsEffortsCmd_RegisteredAsSubcommand sanity-checks that init()
// registered the new command under `rush models`.
func TestModelsEffortsCmd_RegisteredAsSubcommand(t *testing.T) {
	found := false
	for _, sub := range NewModelsCmd().Commands() {
		if strings.HasPrefix(sub.Use, "efforts") {
			found = true
			break
		}
	}
	assert.True(t, found, "modelsEffortsCmd must be registered under modelsCmd")
}

// TestModelsHelp_CrossReferencesEfforts verifies `rush models --help`
// mentions the new command, per the task's requirement to extend (not
// restyle) the existing four-slot Long description.
func TestModelsHelp_CrossReferencesEfforts(t *testing.T) {
	assert.Contains(t, NewModelsCmd().Long, "rush models efforts --help")
}

// TestModelsUnset_RegisteredAsSubcommand sanity-checks that init() registered
// the new command under `rush models`.
func TestModelsUnset_RegisteredAsSubcommand(t *testing.T) {
	found := false
	for _, sub := range NewModelsCmd().Commands() {
		if strings.HasPrefix(sub.Use, "unset") {
			found = true
			break
		}
	}
	assert.True(t, found, "modelsUnsetCmd must be registered under modelsCmd")
}

// TestModelsBump_RegisteredAsSubcommand sanity-checks init() registration.
func TestModelsBump_RegisteredAsSubcommand(t *testing.T) {
	found := false
	for _, sub := range NewModelsCmd().Commands() {
		if sub.Name() == "bump" {
			found = true
			break
		}
	}
	assert.True(t, found, "modelsBumpCmd must be registered under modelsCmd")
}

// TestModelsCmdHelpDocumentsVisibilityFilter is the help contract for the
// family filter (#1171, #1173): the `rush models` help must say which
// families are filtered and at which minimum versions, and the parent
// command must still list the subcommands whose output is filtered.
func TestModelsCmdHelpDocumentsVisibilityFilter(t *testing.T) {
	t.Parallel()

	assert.Contains(t, NewModelsCmd().Long, "gpt-6",
		"models help must name the openai-codex minimum (gpt-6 and newer)")
	assert.Contains(t, NewModelsCmd().Long, "o3", "models help must name the retired o3 family")
	assert.Contains(t, NewModelsCmd().Long, "codex-mini", "models help must name the retired codex-mini family")
	assert.Contains(t, NewModelsCmd().Long, "5.3",
		"models help must name the glm minimum version")
	assert.Contains(t, NewModelsCmd().Long, "Unknown or unparseable model ids are never hidden",
		"models help must state that unparseable ids are shown")

	var listRegistered, cacheRegistered bool
	for _, sub := range NewModelsCmd().Commands() {
		switch sub.Name() {
		case "list":
			listRegistered = true
		case "cache":
			cacheRegistered = true
		}
	}
	assert.True(t, listRegistered, "models list must stay registered under models")
	assert.True(t, cacheRegistered, "models cache must stay registered under models")
}

// TestCollectListModelEntriesHidesSupersededFamilies verifies the
// `rush models` listing itself (configured and catwalk-known providers
// alike) applies the family filter.
func TestCollectListModelEntriesHidesSupersededFamilies(t *testing.T) {
	t.Parallel()

	known := []catwalk.Provider{
		{
			ID:   "zhipu",
			Name: "Zhipu",
			Models: []catwalk.Model{
				{ID: "glm-5.1"},
				{ID: "glm-5.3"},
			},
		},
	}

	entries := collectListModelEntries(visibilityTestConfig(), known, "")
	require.Contains(t, entries, "zai")
	require.Equal(t, []string{"glm-5.3-flash"}, entries["zai"].models,
		"configured providers hide glm below 5.3 and prefixed hidden ids")
	require.True(t, entries["zai"].configured)
	require.Contains(t, entries, "openai-codex")
	require.Equal(t, []string{"gpt-6-luna"}, entries["openai-codex"].models,
		"openai-codex lists only gpt-6 and newer")
	require.Contains(t, entries, "zhipu")
	require.Equal(t, []string{"glm-5.3"}, entries["zhipu"].models,
		"catwalk-known providers get the same glm filter")
}

// visibilityTestConfig returns a config whose zai and openai-codex
// providers carry both hidden and visible models, the fixture for the list
// assertions below.
func visibilityTestConfig() *config.Config {
	cfg := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
	cfg.Providers.Set("zai", config.ProviderConfig{
		ID:   "zai",
		Name: "Z.AI",
		Models: []catwalk.Model{
			{ID: "glm-5.2", ContextWindow: 200_000},
			{ID: "glm-5.3-flash", ContextWindow: 1_000_000, CanReason: true},
			{ID: "cc-glm-5.1", ContextWindow: 200_000},
		},
	})
	cfg.Providers.Set("openai-codex", config.ProviderConfig{
		ID:   "openai-codex",
		Name: "OpenAI Codex",
		Models: []catwalk.Model{
			{ID: "gpt-5.6", ContextWindow: 372_000},
			{ID: "gpt-6-luna", ContextWindow: 1_050_000},
		},
	})
	return cfg
}
