package agent

// getProviderOptions reasoning-effort mapping tests: anthropic/bedrock
// effort plumbing, the upstream first-level fallback, and the fork's ZAI
// and DeepSeek thinking-mode defaults.

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetProviderOptionsReasoningEffort(t *testing.T) {
	// Bedrock is Fantasy's Anthropic under a different provider name; options
	// must land under anthropic.Name so the Anthropic language model picks them up.
	tests := []struct {
		name         string
		providerType catwalk.Type
	}{
		{"anthropic honors reasoning_effort", catwalk.Type(anthropic.Name)},
		{"bedrock honors reasoning_effort", catwalk.Type(bedrock.Name)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := Model{
				CatwalkCfg: catwalk.Model{
					ID:              "claude-opus-4-7",
					CanReason:       true,
					ReasoningLevels: []string{"max"},
				},
				ModelCfg: config.SelectedModel{
					Provider:        "test",
					ReasoningEffort: "max",
				},
			}
			providerCfg := config.ProviderConfig{ID: "test", Type: tc.providerType}

			opts := getProviderOptions(model, providerCfg)

			raw, ok := opts[anthropic.Name]
			require.True(t, ok, "options should be keyed under anthropic.Name for type %q", tc.providerType)
			parsed, ok := raw.(*anthropic.ProviderOptions)
			require.True(t, ok)
			require.NotNil(t, parsed.Effort)
			assert.Equal(t, anthropic.Effort("max"), *parsed.Effort)
		})
	}
}

// Ported from upstream f75435a2: when a reasoning-capable model has no
// default_reasoning_effort configured and the user hasn't selected one,
// getProviderOptions must fall back to the first configured reasoning
// level instead of silently disabling reasoning. Uses the openai branch,
// the same path f75435a2 itself exercises upstream — the fork's own
// ZAI/GLM openaicompat mapping (a fork-owned block, not part of this
// upstream commit) has its own analogous default-on behavior, covered
// separately below.
func TestGetProviderOptionsReasoningEffortFallback(t *testing.T) {
	model := Model{
		CatwalkCfg: catwalk.Model{
			ID:              "gpt-5-test",
			CanReason:       true,
			ReasoningLevels: []string{"high", "max"},
		},
		ModelCfg: config.SelectedModel{
			Provider: "openai",
		},
	}
	providerCfg := config.ProviderConfig{ID: "openai", Type: openai.Name}

	opts := getProviderOptions(model, providerCfg)

	raw, ok := opts[openai.Name]
	require.True(t, ok)
	parsed, ok := raw.(*openai.ProviderOptions)
	require.True(t, ok)
	require.NotNil(t, parsed.ReasoningEffort)
	assert.Equal(t, "high", string(*parsed.ReasoningEffort))
}

// The fork's ZAI/GLM mapping (getProviderOptions, InferenceProviderZAI case)
// has no "unset" state on z.ai's own API — thinking is either on at some
// level or off. An unset ReasoningEffort now defaults thinking ON at "high"
// (z.ai recommends max/high for coding tasks) instead of silently disabling
// reasoning; "off" is the explicit opt-out.
func TestGetProviderOptionsZAIReasoningDefault(t *testing.T) {
	newModel := func(effort string) Model {
		return Model{
			CatwalkCfg: catwalk.Model{
				ID:              "glm-5.2",
				CanReason:       true,
				ReasoningLevels: []string{"high", "max"},
			},
			ModelCfg: config.SelectedModel{
				Provider:        "zai",
				ReasoningEffort: effort,
			},
		}
	}
	providerCfg := config.ProviderConfig{ID: string(catwalk.InferenceProviderZAI), Type: openaicompat.Name}

	extraBody := func(t *testing.T, effort string) map[string]any {
		t.Helper()
		opts := getProviderOptions(newModel(effort), providerCfg)
		raw, ok := opts[openaicompat.Name]
		require.True(t, ok)
		parsed, ok := raw.(*openaicompat.ProviderOptions)
		require.True(t, ok)
		return parsed.ExtraBody
	}

	t.Run("unset defaults to thinking enabled at high", func(t *testing.T) {
		eb := extraBody(t, "")
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "enabled", thinking["type"])
		assert.Equal(t, "high", eb["reasoning_effort"])
	})

	t.Run("xhigh maps to max", func(t *testing.T) {
		eb := extraBody(t, "xhigh")
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "enabled", thinking["type"])
		assert.Equal(t, "max", eb["reasoning_effort"])
	})

	t.Run("off explicitly disables thinking", func(t *testing.T) {
		eb := extraBody(t, "off")
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "disabled", thinking["type"])
		_, hasEffort := eb["reasoning_effort"]
		assert.False(t, hasEffort, "reasoning_effort should not be set when thinking is off")
	})
}

// DeepSeek must keep the fork's ORIGINAL default: an unset ReasoningEffort
// leaves thinking OFF. The ZAI-only "unset → thinking on at high" default
// (added in 28ec4145) deliberately does not apply to DeepSeek — this test
// pins that separation so the two providers can't drift back together.
func TestGetProviderOptionsDeepSeekReasoningDefault(t *testing.T) {
	newModel := func(effort string, think bool) Model {
		return Model{
			CatwalkCfg: catwalk.Model{
				ID:        "deepseek-reasoner",
				CanReason: true,
			},
			ModelCfg: config.SelectedModel{
				Provider:        "deepseek",
				ReasoningEffort: effort,
				Think:           think,
			},
		}
	}
	providerCfg := config.ProviderConfig{ID: string(catwalk.InferenceProviderDeepSeek), Type: openaicompat.Name}

	extraBody := func(t *testing.T, effort string, think bool) map[string]any {
		t.Helper()
		opts := getProviderOptions(newModel(effort, think), providerCfg)
		raw, ok := opts[openaicompat.Name]
		require.True(t, ok)
		parsed, ok := raw.(*openaicompat.ProviderOptions)
		require.True(t, ok)
		return parsed.ExtraBody
	}

	t.Run("unset leaves thinking disabled (old behavior)", func(t *testing.T) {
		eb := extraBody(t, "", false)
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "disabled", thinking["type"])
		_, hasEffort := eb["reasoning_effort"]
		assert.False(t, hasEffort, "reasoning_effort must not be set when effort is unset")
	})

	t.Run("Think enables thinking without explicit effort", func(t *testing.T) {
		eb := extraBody(t, "", true)
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "enabled", thinking["type"])
		_, hasEffort := eb["reasoning_effort"]
		assert.False(t, hasEffort, "reasoning_effort stays unset when only Think is set")
	})

	t.Run("explicit effort enables thinking and maps high", func(t *testing.T) {
		eb := extraBody(t, "high", false)
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "enabled", thinking["type"])
		assert.Equal(t, "high", eb["reasoning_effort"])
	})

	t.Run("xhigh maps to max", func(t *testing.T) {
		eb := extraBody(t, "xhigh", false)
		thinking, ok := eb["thinking"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "enabled", thinking["type"])
		assert.Equal(t, "max", eb["reasoning_effort"])
	})
}
