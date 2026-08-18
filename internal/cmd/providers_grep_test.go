// Provider grep filtering tests: matchesGrep behavior (id, name, type,
// case-insensitivity, empty pattern) and the providers-grep level
// TestProvidersGrep_Filters.
package cmd

import (
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProvidersGrep_Filters(t *testing.T) {
	t.Parallel()
	providers := map[string]config.ProviderConfig{
		"openai":    {Name: "OpenAI", Type: catwalk.TypeOpenAI},
		"anthropic": {Name: "Anthropic", Type: catwalk.TypeAnthropic},
		"zai":       {Name: "Z.AI", Type: catwalk.TypeOpenAICompat},
	}

	matched := 0
	for id, p := range providers {
		if matchesGrep(id, p, "anthropic") {
			matched++
		}
	}
	assert.Equal(t, 1, matched, "only anthropic should match 'anthropic'")

	matched = 0
	for id, p := range providers {
		if matchesGrep(id, p, "z") {
			matched++
		}
	}
	assert.Equal(t, 1, matched, "only zai should match 'z'")
}

func TestMatchesGrep(t *testing.T) {
	tests := []struct {
		id       string
		provider config.ProviderConfig
		pattern  string
		expect   bool
	}{
		{
			id:       "openai",
			provider: config.ProviderConfig{Name: "OpenAI", Type: catwalk.TypeOpenAI},
			pattern:  "openai",
			expect:   true,
		},
		{
			id:       "openai",
			provider: config.ProviderConfig{Name: "OpenAI", Type: catwalk.TypeOpenAI},
			pattern:  "gpt",
			expect:   false,
		},
		{
			id:       "zai",
			provider: config.ProviderConfig{Name: "Z.AI", Type: catwalk.TypeOpenAICompat},
			pattern:  "z.ai",
			expect:   true,
		},
		{
			id:       "anthropic",
			provider: config.ProviderConfig{Name: "Anthropic", Type: catwalk.TypeAnthropic},
			pattern:  "anthropic",
			expect:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.id+"_"+tt.pattern, func(t *testing.T) {
			result := matchesGrep(tt.id, tt.provider, strings.ToLower(tt.pattern))
			require.Equal(t, tt.expect, result)
		})
	}
}

func TestMatchesGrep_EmptyPattern(t *testing.T) {
	p := config.ProviderConfig{Name: "OpenAI", Type: catwalk.TypeOpenAI}
	assert.True(t, matchesGrep("openai", p, ""), "empty pattern should match everything")
}

func TestMatchesGrep_CaseInsensitive(t *testing.T) {
	p := config.ProviderConfig{Name: "OpenAI", Type: catwalk.TypeOpenAI}
	assert.True(t, matchesGrep("openai", p, "openai"))
	assert.True(t, matchesGrep("OPENAI", p, "openai"))
}

func TestMatchesGrep_ByType(t *testing.T) {
	p := config.ProviderConfig{Name: "My Provider", Type: catwalk.TypeAnthropic}
	assert.True(t, matchesGrep("custom-id", p, "anthropic"))
	assert.False(t, matchesGrep("custom-id", p, "gemini"))
}

func TestMatchesGrep_ByName(t *testing.T) {
	p := config.ProviderConfig{Name: "Z.AI", Type: catwalk.TypeOpenAICompat}
	assert.True(t, matchesGrep("zai", p, "z"))
	assert.True(t, matchesGrep("zai", p, "openai"))
}
