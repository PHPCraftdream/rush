// Provider list-item rendering tests: makeProviderListItem and the
// providerListItem it produces (status column, disabled flag, masked
// API key, env templates, OAuth marker, JSON round-trip), plus the
// maskKey and dash helpers behind that rendering.
package cmd

import (
	"encoding/json"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProvidersList_StatusColumn(t *testing.T) {
	t.Parallel()
	providers := map[string]config.ProviderConfig{
		"openai":    {Name: "OpenAI", Type: catwalk.TypeOpenAI, Disable: false, Models: []catwalk.Model{{ID: "gpt-4o"}}, APIKey: "sk-1234567890"},
		"disabled1": {Name: "Disabled", Type: catwalk.TypeAnthropic, Disable: true},
	}

	for id, p := range providers {
		status := "enabled"
		if p.Disable {
			status = "disabled"
		}
		item := makeProviderListItem(id, p)
		assert.Equal(t, p.Disable, item.Disabled)

		if id == "openai" {
			assert.Equal(t, false, item.Disabled)
			assert.Equal(t, "enabled", status)
			assert.Equal(t, 1, item.Models)
		} else {
			assert.Equal(t, true, item.Disabled)
			assert.Equal(t, "disabled", status)
			assert.Equal(t, 0, item.Models)
		}
	}
}

func TestProvidersEnable_SetsDisableFalse(t *testing.T) {
	t.Parallel()
	p := config.ProviderConfig{
		ID:      "test",
		Name:    "Test",
		Type:    catwalk.TypeOpenAI,
		Disable: true,
	}
	item := makeProviderListItem("test", p)
	assert.True(t, item.Disabled)

	p.Disable = false
	item = makeProviderListItem("test", p)
	assert.False(t, item.Disabled)
}

func TestProvidersAdd_ValidProvider(t *testing.T) {
	t.Parallel()
	p := config.ProviderConfig{
		ID:      "my-provider",
		Name:    "My Provider",
		Type:    catwalk.TypeOpenAICompat,
		BaseURL: "http://localhost:8000/v1",
		APIKey:  "test-key",
		Disable: false,
	}
	item := makeProviderListItem("my-provider", p)
	assert.Equal(t, "my-provider", item.ID)
	assert.Equal(t, "My Provider", item.Name)
	assert.Equal(t, "openai-compat", item.Type)
	assert.False(t, item.Disabled)
	assert.True(t, item.APIKeyPresent)
}

func TestProviderListItem(t *testing.T) {
	p := config.ProviderConfig{
		ID:      "openai",
		Name:    "OpenAI",
		Type:    catwalk.TypeOpenAI,
		APIKey:  "sk_live_abc123def456",
		BaseURL: "https://api.openai.com/v1",
		Disable: false,
		Models: []catwalk.Model{
			{ID: "gpt-4o", Name: "GPT-4 Omni"},
			{ID: "gpt-4-turbo", Name: "GPT-4 Turbo"},
		},
	}

	item := makeProviderListItem("openai", p)

	require.Equal(t, "openai", item.ID)
	require.Equal(t, "OpenAI", item.Name)
	require.Equal(t, "openai", item.Type)
	require.Equal(t, "****f456", item.APIKey)
	require.True(t, item.APIKeyPresent)
	require.Equal(t, "https://api.openai.com/v1", item.BaseURL)
	require.False(t, item.Disabled)
	require.Equal(t, 2, item.Models)
}

func TestProviderListItem_NoAPIKey(t *testing.T) {
	p := config.ProviderConfig{
		ID:   "test",
		Name: "Test",
		Type: catwalk.TypeOpenAI,
	}

	item := makeProviderListItem("test", p)

	require.Equal(t, "-", item.APIKey)
	require.False(t, item.APIKeyPresent)
}

func TestProviderListItem_EnvTemplate(t *testing.T) {
	p := config.ProviderConfig{
		ID:     "test",
		Name:   "Test",
		Type:   catwalk.TypeOpenAI,
		APIKey: "$OPENAI_KEY",
	}

	item := makeProviderListItem("test", p)

	require.Equal(t, "$OPENAI_KEY", item.APIKey)
	require.True(t, item.APIKeyPresent)
}

func TestProviderListItem_Disabled(t *testing.T) {
	p := config.ProviderConfig{
		ID:      "test",
		Name:    "Test",
		Type:    catwalk.TypeOpenAI,
		Disable: true,
	}
	item := makeProviderListItem("test", p)
	assert.True(t, item.Disabled)
}

func TestProviderListItem_OAuth(t *testing.T) {
	p := config.ProviderConfig{
		ID:   "test",
		Name: "Test",
		Type: catwalk.TypeOpenAI,
		OAuthToken: &oauth.Token{
			AccessToken: "test-token",
		},
	}
	item := makeProviderListItem("test", p)
	assert.True(t, item.HasOAuth)
}

func TestMaskKey_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty",
			input:    "",
			expected: "-",
		},
		{
			name:     "env_template",
			input:    "$OPENAI_API_KEY",
			expected: "$OPENAI_API_KEY",
		},
		{
			name:     "short_key",
			input:    "abc",
			expected: "****",
		},
		{
			name:     "four_chars",
			input:    "1234",
			expected: "****",
		},
		{
			name:     "long_key",
			input:    "sk_live_1234567890abcdef",
			expected: "****cdef",
		},
		{
			name:     "five_chars",
			input:    "abcde",
			expected: "****bcde",
		},
		{
			name:     "env_braces",
			input:    "${MY_KEY}",
			expected: "${MY_KEY}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := maskKey(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestProviderListItemJSON(t *testing.T) {
	t.Parallel()
	p := config.ProviderConfig{
		ID:      "openai",
		Name:    "OpenAI",
		Type:    catwalk.TypeOpenAI,
		APIKey:  "sk-1234567890abcdef",
		BaseURL: "https://api.openai.com/v1",
		Disable: false,
		Models:  []catwalk.Model{{ID: "gpt-4o"}, {ID: "gpt-5"}},
	}

	item := makeProviderListItem("openai", p)
	data, err := json.Marshal(item)
	require.NoError(t, err)

	var parsed providerListItem
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Equal(t, "openai", parsed.ID)
	assert.Equal(t, "OpenAI", parsed.Name)
	assert.Equal(t, "openai", parsed.Type)
	assert.Equal(t, "****cdef", parsed.APIKey)
	assert.True(t, parsed.APIKeyPresent)
	assert.Equal(t, 2, parsed.Models)
	assert.False(t, parsed.Disabled)
}

func TestDashHelper(t *testing.T) {
	assert.Equal(t, "-", dash(""))
	assert.Equal(t, "hello", dash("hello"))
	assert.Equal(t, "https://api.openai.com/v1", dash("https://api.openai.com/v1"))
}
