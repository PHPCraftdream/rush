package modelatoms

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitModelEffort(t *testing.T) {
	cases := []struct {
		in         string
		wantModel  string
		wantEffort string
	}{
		{"", "", ""},
		{"gpt-5", "gpt-5", ""},
		{"openai/gpt-5", "openai/gpt-5", ""},
		{"gpt-5@high", "gpt-5", "high"},
		{"openai/gpt-5@low", "openai/gpt-5", "low"},
		{"@only-effort", "", "only-effort"}, // edge: leading @
		{"weird@a@b", "weird@a", "b"},       // last @ wins
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotModel, gotEffort := SplitModelEffort(tc.in)
			assert.Equal(t, tc.wantModel, gotModel, "model part")
			assert.Equal(t, tc.wantEffort, gotEffort, "effort part")
		})
	}
}

func TestSplitEffortTierAndValidate(t *testing.T) {
	cases := []struct{ in, effort, tier string }{
		{"", "", ""}, {"high", "high", ""}, {"high/fast", "high", "fast"}, {"/fast", "", "fast"},
	}
	for _, tc := range cases {
		effort, tier := splitEffortTier(tc.in)
		assert.Equal(t, tc.effort, effort, tc.in)
		assert.Equal(t, tc.tier, tier, tc.in)
		assert.Equal(t, tc.in, EffortWithTier(effort, tier), tc.in)
	}
	require.NoError(t, validateServiceTier("openai-codex", ""))
	require.NoError(t, validateServiceTier("openai-codex", "fast"))
	require.ErrorContains(t, validateServiceTier("zai", "fast"), "only supported on openai-codex")
	require.ErrorContains(t, validateServiceTier("openai-codex", "turbo"), "unknown service tier")

	resolve := func(v string) (string, string, bool, error) { return "openai-codex", "gpt-6.2-sol", true, nil }
	sel, _, err := ParseAtomOrRaw("openai-codex/gpt-6.2-sol@high/fast", resolve)
	require.NoError(t, err)
	assert.Equal(t, config.SelectedModel{Provider: "openai-codex", Model: "gpt-6.2-sol", ReasoningEffort: "high", ServiceTier: "fast"}, sel)
	_, _, err = ParseAtomOrRaw("openai-codex/gpt-6.2-sol@high/turbo", resolve)
	require.Error(t, err)
}
