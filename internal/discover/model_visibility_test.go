package discover

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestModelVisibleFamilyTable is the id-to-visibility oracle for
// modelVisibilityRules: openai-codex shows gpt-6 and newer; glm models are
// hidden below 5.3 for every provider; unparseable and unknown ids are
// always shown.
func TestModelVisibleFamilyTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		id       string
		visible  bool
	}{
		// openai-codex: gpt-* >= 6.
		{"openai-codex", "gpt-6", true},
		{"openai-codex", "gpt-6-luna", true},
		{"openai-codex", "gpt-6-sol", true},
		{"openai-codex", "gpt-6-astra", true},
		{"openai-codex", "gpt-6.1", true},
		{"openai-codex", "gpt-6-astra-wm", true},
		{"openai-codex", "gpt-5.6", false},
		{"openai-codex", "gpt-5.6-luna", false},
		{"openai-codex", "gpt-5.5", false},
		{"openai-codex", "gpt-4", false},
		{"openai-codex", "gpt-4o", false},
		// Version suffixes do not change the parsed version.
		{"openai-codex", "gpt-5.5-turbo", false},
		// Retired families without a version scheme.
		{"openai-codex", "o3", false},
		{"openai-codex", "o3-mini", false},
		{"openai-codex", "codex-mini", false},
		{"openai-codex", "codex-mini-latest", false},
		{"openai-codex", "codex-model", true},
		// Unparseable and unknown ids are never lost.
		{"openai-codex", "gpt-x", true},
		{"openai-codex", "gpt", true},
		{"openai-codex", "gpt6", true},
		{"openai-codex", "some-future-model", true},
		// The rule is scoped to openai-codex: the regular openai provider
		// keeps its gpt-4/5.x catalog.
		{"openai", "gpt-5.6", true},
		{"openai", "gpt-4o", true},
		{"openai", "o3", true},
		{"openai", "codex-mini", true},
		// glm-* >= 5.3 for every provider.
		{"zai", "glm-5.3", true},
		{"zai", "glm-5.3-flash", true},
		{"zai", "glm-5.4", true},
		{"zai", "glm-5", false},
		{"zai", "glm-5-turbo", false},
		{"zai", "glm-5.1", false},
		{"zai", "glm-5.2", false},
		{"zai", "glm-4.5", false},
		{"zai", "glm-4.6", false},
		{"zai", "glm-4.7", false},
		{"zai", "glm-4.5v", false},
		{"zhipu", "glm-5.1", false},
		{"aihubmix", "cc-glm-5.1", false},
		{"aihubmix", "cc-glm-5.3-turbo", true},
		{"opencode-zen", "glm-x", true},
		// The family token must start the id or follow a "-": a longer word
		// that merely ends in it is another family.
		{"zai", "superglm-5.1", true},
		{"openai-codex", "xgpt-5", true},
	}

	for _, test := range tests {
		require.Equal(t, test.visible, ModelVisible(test.provider, test.id),
			"ModelVisible(%q, %q)", test.provider, test.id)
	}
}
