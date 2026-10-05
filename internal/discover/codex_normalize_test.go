package discover

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

// TestNormalizeCodexModelRaisesStaleCachedContext covers the entries a catalog
// cache written before the documented GPT-6 window was applied still carries
// (272000 for every gpt-6 model, observed in a real cache file). The family is
// gpt-6 and every later gpt-6.N release of astra, sol and luna; a larger
// reported value is kept, models outside the family are left alone, and the
// default output budget never exceeds the window.
//
// Revert-check: return the model unchanged from NormalizeCodexModel and every
// raised row goes red; narrow codexGPT6Documented back to the three exact ids
// and the gpt-6.1-sol row goes red.
func TestNormalizeCodexModelRaisesStaleCachedContext(t *testing.T) {
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
			got := NormalizeCodexModel(test.model)
			require.Equal(t, test.wantWindow, got.ContextWindow)
			require.LessOrEqual(t, got.DefaultMaxTokens, got.ContextWindow)
		})
	}
}
