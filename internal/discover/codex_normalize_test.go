// The documented-facts overlay replaces the former NormalizeCodexModel: the
// same entries a catalog cache written by an older Rush still carries
// (272000 for every gpt-6 model, observed in a real cache file) must be
// raised by ApplyModelFacts at config load.
//
// Revert-check: drop the openai-codex rows from modelFactRules (or the final
// pass in loadProviders) and every raised row goes red; narrow the GPT-6
// predicate back to the three exact ids and the gpt-6.1-sol row goes red.
package discover

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestApplyModelFactsRaisesStaleCachedContext(t *testing.T) {
	tests := []struct {
		name       string
		model      catwalk.Model
		wantWindow int64
	}{
		{"luna", catwalk.Model{ID: "gpt-6-luna", ContextWindow: 272000, DefaultMaxTokens: 128000}, codexGPT6ContextWindow},
		{"sol", catwalk.Model{ID: "gpt-6-sol", ContextWindow: 272000, DefaultMaxTokens: 128000}, codexGPT6ContextWindow},
		{"astra", catwalk.Model{ID: "gpt-6-astra", ContextWindow: 272000, DefaultMaxTokens: 128000}, codexGPT6ContextWindow},
		{"later release", catwalk.Model{ID: "gpt-6.1-sol", ContextWindow: 272000, DefaultMaxTokens: 128000}, codexGPT6ContextWindow},
		{"wm variant", catwalk.Model{ID: "gpt-6-astra-wm", ContextWindow: 272000, DefaultMaxTokens: 128000}, codexGPT6ContextWindow},
		{"larger reported value is kept", catwalk.Model{ID: "gpt-6-sol", ContextWindow: 2_000_000, DefaultMaxTokens: 128000}, 2_000_000},
		{"outside the family", catwalk.Model{ID: "gpt-5.5", ContextWindow: 272000, DefaultMaxTokens: 128000}, 272000},
		{"gpt-6 without a documented tier", catwalk.Model{ID: "gpt-6-mini", ContextWindow: 272000, DefaultMaxTokens: 128000}, 272000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			models := []catwalk.Model{test.model}
			ApplyModelFacts("openai-codex", models, nil)
			require.Equal(t, test.wantWindow, models[0].ContextWindow)
			require.LessOrEqual(t, models[0].DefaultMaxTokens, models[0].ContextWindow)
		})
	}
}
