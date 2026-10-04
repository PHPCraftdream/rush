package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/discover"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelsCmdHelpDocumentsVisibilityFilter is the help contract for the
// family filter (#1171, #1173): the `rush models` help must say which
// families are filtered and at which minimum versions, and the parent
// command must still list the subcommands whose output is filtered.
func TestModelsCmdHelpDocumentsVisibilityFilter(t *testing.T) {
	t.Parallel()

	assert.Contains(t, modelsCmd.Long, "gpt-6",
		"models help must name the openai-codex minimum (gpt-6 and newer)")
	assert.Contains(t, modelsCmd.Long, "o3", "models help must name the retired o3 family")
	assert.Contains(t, modelsCmd.Long, "codex-mini", "models help must name the retired codex-mini family")
	assert.Contains(t, modelsCmd.Long, "5.3",
		"models help must name the glm minimum version")
	assert.Contains(t, modelsCmd.Long, "Unknown or unparseable model ids are never hidden",
		"models help must state that unparseable ids are shown")

	var listRegistered, cacheRegistered bool
	for _, sub := range modelsCmd.Commands() {
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

// TestProvidersFetchModelsHelpDocumentsVisibilityFilter is the help
// contract for the `providers fetch-models` surface of the same filter.
func TestProvidersFetchModelsHelpDocumentsVisibilityFilter(t *testing.T) {
	t.Parallel()

	assert.Contains(t, providersFetchModelsCmd.Long, "gpt-6",
		"fetch-models help must name the openai-codex minimum")
	assert.Contains(t, providersFetchModelsCmd.Long, "5.3",
		"fetch-models help must name the glm minimum version")
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

// TestRenderOtherModelsBlockHidesSupersededFamilies verifies the raw
// provider/model section of `rush models list` applies the family filter.
func TestRenderOtherModelsBlockHidesSupersededFamilies(t *testing.T) {
	t.Parallel()

	require.True(t, !discover.ModelVisible("zai", "glm-5.2"),
		"precondition: the fixture carries a hidden glm model")

	out := renderOtherModelsBlock(visibilityTestConfig())
	assert.Contains(t, out, "zai/glm-5.3-flash", "visible glm models are listed")
	assert.Contains(t, out, "openai-codex/gpt-6-luna", "visible gpt-6 models are listed")
	assert.NotContains(t, out, "glm-5.2", "glm models below 5.3 are hidden")
	assert.NotContains(t, out, "cc-glm-5.1", "prefixed glm ids below 5.3 are hidden")
	assert.NotContains(t, out, "gpt-5.6", "pre-6 gpt models are hidden for openai-codex")
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

// TestEmitModelsListJSONHidesSupersededFamilies verifies the --json surface
// of `rush models list` applies the same filter. Runs serially because it
// swaps os.Stdout for a pipe.
func TestEmitModelsListJSONHidesSupersededFamilies(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	origStdout := os.Stdout
	os.Stdout = w
	emitErr := emitModelsListJSON(visibilityTestConfig())
	os.Stdout = origStdout
	require.NoError(t, w.Close())
	require.NoError(t, emitErr)

	data, err := io.ReadAll(r)
	require.NoError(t, err)

	var payload struct {
		OtherModels []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"other_models"`
	}
	require.NoError(t, json.Unmarshal(data, &payload))

	listed := make([]string, 0, len(payload.OtherModels))
	for _, model := range payload.OtherModels {
		listed = append(listed, model.Provider+"/"+model.Model)
	}
	require.Equal(t, []string{"openai-codex/gpt-6-luna", "zai/glm-5.3-flash"}, listed,
		"the JSON surface must hide glm below 5.3 and pre-6 gpt exactly like the text surface")
}

// TestFetchModelsHidesSupersededFamilies covers the `providers fetch-models` /
// `providers update` path: a provider that serves old GLM ids next to a current
// one returns only the visible ones, and an unparseable id is kept.
//
// Revert-check: make fetchModels return the unfiltered list (drop the
// discover.ModelVisible check) and the glm-5.2 / glm-4.5v ids come back.
func TestFetchModelsHidesSupersededFamilies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-4.5v"},{"id":"glm-5.2"},{"id":"glm-5.3"},{"id":"glm-x"}]}`))
	}))
	defer server.Close()

	// a is only needed to resolve "$VAR" templates; none are used here.
	models, _, err := fetchModels(nil, config.ProviderConfig{
		ID: "zai", Type: "openai-compat", BaseURL: server.URL,
	})
	require.NoError(t, err)

	var ids []string
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	require.Equal(t, []string{"glm-5.3", "glm-x"}, ids)
}
