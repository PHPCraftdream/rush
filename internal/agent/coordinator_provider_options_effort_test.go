package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// z.ai and DeepSeek carry their effort in extra_body, in wire values the
// OpenAI vocabulary lacks ("max"). Fantasy validates the TOP-LEVEL
// reasoning_effort against that vocabulary and fails the whole call with
// "reasoning model `max` not supported", so such a value must never also ride
// there. It made every reviewer pass at zai/glm-5.3-flash@max fail (8 of 8
// agent runs on 2026-10-05); values fantasy accepts keep working as before.
//
// Revert-check: drop the fantasyAcceptsReasoningEffort guard in
// getProviderOptions and every "max" row goes red (top-level "max").
func TestGetProviderOptions_WireOnlyEffortNeverReachesFantasyTopLevel(t *testing.T) {
	zai := config.ProviderConfig{ID: string(catwalk.InferenceProviderZAI), Type: openaicompat.Name}
	zaiLive := config.ProviderConfig{
		ID: string(catwalk.InferenceProviderZAI), Type: openaicompat.Name,
		LiveEfforts: map[string]config.ModelEffortInfo{
			"glm-live": {Levels: []string{"none", "low", "high", "max"}},
		},
	}
	deepseek := config.ProviderConfig{ID: string(catwalk.InferenceProviderDeepSeek), Type: openaicompat.Name}

	for _, tc := range []struct {
		name     string
		provider config.ProviderConfig
		model    string
		levels   []string
		effort   string
		wantWire string // extra_body.reasoning_effort
		wantTop  string // top-level reasoning_effort ("" = absent)
	}{
		{"glm-5.3-flash max", zai, "glm-5.3-flash", []string{"low", "high", "max"}, "max", "max", ""},
		{"glm-5.3 max", zai, "glm-5.3", []string{"low", "high", "max"}, "max", "max", ""},
		{"glm-5.2 max", zai, "glm-5.2", []string{"high", "max"}, "max", "max", ""},
		{"live-verified model max", zaiLive, "glm-live", []string{"none", "low", "high", "max"}, "max", "max", ""},
		{"deepseek max", deepseek, "deepseek-reasoner", []string{"high", "max"}, "max", "max", ""},
		// Values fantasy accepts are untouched.
		{"glm-5.3-flash low keeps the top-level value", zai, "glm-5.3-flash", []string{"low", "high", "max"}, "low", "low", "low"},
		{"glm-5.3 high keeps the top-level value", zai, "glm-5.3", []string{"low", "high", "max"}, "high", "high", "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := Model{
				CatwalkCfg: catwalk.Model{ID: tc.model, CanReason: true, ReasoningLevels: tc.levels},
				ModelCfg:   config.SelectedModel{Provider: tc.provider.ID, Model: tc.model, ReasoningEffort: tc.effort},
			}
			raw := getProviderOptions("test-session", model, tc.provider)[openaicompat.Name]
			parsed, ok := raw.(*openaicompat.ProviderOptions)
			require.True(t, ok)
			require.Equal(t, tc.wantWire, parsed.ExtraBody["reasoning_effort"], "the wire value must still reach extra_body")
			if tc.wantTop == "" {
				require.Nil(t, parsed.ReasoningEffort, "fantasy would reject a top-level %q", tc.effort)
			} else {
				require.NotNil(t, parsed.ReasoningEffort)
				require.Equal(t, tc.wantTop, string(*parsed.ReasoningEffort))
			}
		})
	}
}
