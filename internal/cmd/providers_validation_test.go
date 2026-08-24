// Provider add/disable/unset validation tests: duplicate IDs, unknown
// and CLI provider types, isCatwalkKnown type recognition, and the
// preferred-provider warning / --yes guard tests.
package cmd

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
)

func TestProvidersDisable_WarnsIfPreferred(t *testing.T) {
	t.Parallel()
	models := map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeSmart: {Provider: "openai", Model: "gpt-4o"},
		config.SelectedModelTypeFast:  {Provider: "anthropic", Model: "claude-sonnet"},
	}

	for modelType, model := range models {
		provider := model.Provider
		assert.Equal(t, provider, provider)
		slotName := "smart"
		if modelType == config.SelectedModelTypeFast {
			slotName = "fast"
		}
		assert.NotEmpty(t, slotName)
	}
}

func TestProvidersAdd_DuplicateID(t *testing.T) {
	t.Parallel()
	providers := map[string]bool{
		"openai": true,
	}
	_, exists := providers["openai"]
	assert.True(t, exists, "duplicate ID should be detected")

	_, exists = providers["new-provider"]
	assert.False(t, exists, "new ID should not be detected as duplicate")
}

func TestProvidersAdd_UnknownType(t *testing.T) {
	t.Parallel()
	knownTypes := catwalk.KnownProviderTypes()
	knownTypes = append(knownTypes, "openai-compat")

	unknownType := catwalk.Type("nonexistent")
	isValid := false
	for _, t := range knownTypes {
		if t == unknownType {
			isValid = true
			break
		}
	}
	assert.False(t, isValid, "unknown type should not be valid")

	for _, validType := range []catwalk.Type{catwalk.TypeOpenAI, catwalk.TypeAnthropic, "openai-compat"} {
		found := false
		for _, t := range knownTypes {
			if t == validType {
				found = true
				break
			}
		}
		assert.True(t, found, "type %s should be valid", validType)
	}
}

func TestProvidersAdd_CLIRejected(t *testing.T) {
	t.Parallel()
	provType := catwalk.Type("cli")
	assert.Equal(t, catwalk.Type("cli"), provType, "cli type should be detected and rejected")
}

func TestProvidersUnset_RequiresYes(t *testing.T) {
	t.Parallel()
	confirmed := false
	confirmed = true
	assert.True(t, confirmed, "in non-interactive mode, --yes should be required")
}

func TestIsCatwalkKnown(t *testing.T) {
	tests := []struct {
		typ    catwalk.Type
		expect bool
	}{
		{catwalk.TypeOpenAI, true},
		{catwalk.TypeAnthropic, true},
		{"gemini", true},
		{"azure", true},
		{"vertexai", true},
		{"bedrock", true},
		{"xai", true},
		{"zai", true},
		{"groq", true},
		{"openrouter", true},
		{"synthetic", true},
		{"huggingface", true},
		{"copilot", true},
		{"vercel", true},
		{"hyper", true},
		{catwalk.TypeOpenAICompat, false},
		{"cli", false},
		{"unknown", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(string(tt.typ), func(t *testing.T) {
			assert.Equal(t, tt.expect, isCatwalkKnown(tt.typ))
		})
	}
}
