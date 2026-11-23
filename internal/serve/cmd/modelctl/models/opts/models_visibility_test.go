package modelopts

import (
	"encoding/json"
	"io"
	"os"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/config/providers/discover"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
