// Table test for the documented-facts table: every row must produce its
// documented window through ApplyModelFacts, and the zai row must also
// ensure the GLM-5.3 entry exists.
//
// Revert-check: change any contextWindow/defaultMaxTokens in modelFactRules,
// drop a row, or reorder the rows so an earlier one shadows a later one, and
// the matching case goes red.
package discover

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestApplyModelFactsEveryTableRow(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		model      catwalk.Model
		wantWindow int64
		wantMax    int64
	}{
		// openai-codex rows, in table order.
		{"gpt-6 documented tier floors", "openai-codex", catwalk.Model{ID: "gpt-6-luna", ContextWindow: 272000, DefaultMaxTokens: 0}, 1_050_000, 128_000},
		{"gpt-6 larger reported value kept", "openai-codex", catwalk.Model{ID: "gpt-6-sol", ContextWindow: 2_000_000}, 2_000_000, 128_000},
		{"gpt-5.6 luna floors", "openai-codex", catwalk.Model{ID: "gpt-5.6-luna", ContextWindow: 0}, 1_000_000, 128_000},
		{"gpt-5.6 default fills", "openai-codex", catwalk.Model{ID: "gpt-5.6", ContextWindow: 0}, 372_000, 128_000},
		{"gpt-6 without tier defaults", "openai-codex", catwalk.Model{ID: "gpt-6-orion", ContextWindow: 0}, 1_050_000, 128_000},
		{"gpt-5.6- defaults", "openai-codex", catwalk.Model{ID: "gpt-5.6-mini", ContextWindow: 0}, 372_000, 128_000},
		{"unknown codex id defaults", "openai-codex", catwalk.Model{ID: "codex-model", ContextWindow: 0}, 272_000, 128_000},
		{"unknown codex id keeps reported", "openai-codex", catwalk.Model{ID: "codex-model", ContextWindow: 96_000}, 96_000, 96_000},
		// zai row.
		{"glm-5.3 floors zero", "zai", catwalk.Model{ID: "glm-5.3"}, 1_000_000, 131_072},
		{"glm-5.3-flash floors zero", "zai", catwalk.Model{ID: "glm-5.3-flash"}, 1_000_000, 131_072},
		{"glm-5.3-flashx floors zero", "zai", catwalk.Model{ID: "glm-5.3-flashx"}, 1_000_000, 131_072},
		{"glm-5.3 keeps a larger window", "zai", catwalk.Model{ID: "glm-5.3", ContextWindow: 2_000_000}, 2_000_000, 131_072},
		{"glm-5.3 never lowers", "zai", catwalk.Model{ID: "glm-5.3", ContextWindow: 500_000}, 1_000_000, 131_072},
		{"glm-4.7 has no row", "zai", catwalk.Model{ID: "glm-4.7", ContextWindow: 0}, 0, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			models := []catwalk.Model{test.model}
			ApplyModelFacts(test.provider, models, nil)
			require.Equal(t, test.wantWindow, models[0].ContextWindow)
			require.Equal(t, test.wantMax, models[0].DefaultMaxTokens)
		})
	}
}

func TestApplyModelFactsUserValuesWin(t *testing.T) {
	models := []catwalk.Model{
		{ID: "gpt-6-luna", ContextWindow: 100000, DefaultMaxTokens: 4096},
		{ID: "glm-5.3", ContextWindow: 123000},
	}
	userSet := map[string]UserModelFacts{
		"gpt-6-luna": {ContextWindow: true, DefaultMaxTokens: true},
		"glm-5.3":    {ContextWindow: true},
	}
	ApplyModelFacts("zai", models, userSet)
	require.Equal(t, int64(100000), models[0].ContextWindow, "a user-set window below the floor must win")
	require.Equal(t, int64(4096), models[0].DefaultMaxTokens)
	require.Equal(t, int64(123000), models[1].ContextWindow, "a user-set window must win even on a floored row")
	require.Equal(t, int64(123000), models[1].DefaultMaxTokens,
		"the filled budget is clamped to the final window")
}

func TestApplyModelFactsEnsuresGLM53Present(t *testing.T) {
	models := []catwalk.Model{{ID: "glm-5.3-flash"}}
	out := ApplyModelFacts("zai", models, nil)
	require.Len(t, out, 2)
	var glm53 *catwalk.Model
	for i := range out {
		if out[i].ID == "glm-5.3" {
			glm53 = &out[i]
		}
	}
	require.NotNil(t, glm53)
	require.Equal(t, "GLM-5.3", glm53.Name)
	require.True(t, glm53.CanReason)
	require.Equal(t, GLM53ReasoningLevels, glm53.ReasoningLevels)
	require.Equal(t, "high", glm53.DefaultReasoningEffort)

	// Ensure-present template preserves positive user overrides per field.
	out = ApplyModelFacts("zai", []catwalk.Model{{ID: "glm-5.3-flash"}}, map[string]UserModelFacts{
		"glm-5.3": {ContextWindow: true, DefaultMaxTokens: true},
	})
	require.Len(t, out, 2)
	for i := range out {
		if out[i].ID == "glm-5.3" {
			require.Zero(t, out[i].ContextWindow, "marked user window is not synthesized from the template")
			require.Zero(t, out[i].DefaultMaxTokens, "marked user max is not synthesized from the template")
		}
	}

	// Present already: never duplicated.
	out = ApplyModelFacts("zai", []catwalk.Model{{ID: "glm-5.3"}}, nil)
	require.Len(t, out, 1)

	// Other providers get no ensure-present template.
	out = ApplyModelFacts("openai-codex", []catwalk.Model{{ID: "gpt-6-luna"}}, nil)
	require.Len(t, out, 1)
}

func TestLookupModelFacts(t *testing.T) {
	cw, mt := LookupModelFacts("zai", "glm-5.3-flash")
	require.Equal(t, int64(1_000_000), cw)
	require.Equal(t, int64(131_072), mt)
	cw, mt = LookupModelFacts("zai", "glm-4.7")
	require.Zero(t, cw)
	require.Zero(t, mt)
	cw, mt = LookupModelFacts("openai-codex", "gpt-6-astra")
	require.Equal(t, codexGPT6ContextWindow, cw)
	require.Equal(t, codexDefaultMaxTokens, mt)
	cw, mt = LookupModelFacts("unknown-provider", "anything")
	require.Zero(t, cw)
	require.Zero(t, mt)
}
