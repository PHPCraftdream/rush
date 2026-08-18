// configureProviders core tests: known/custom provider merging and
// overrides, Bedrock/VertexAI credential gating, provider IDs,
// enabled/configured state, agent tool allow-lists, and the shared
// testStore helper.

package config

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/agent/cliprovider"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testStore wraps a Config in a minimal ConfigStore for testing.
func testStore(cfg *Config) *ConfigStore {
	return NewTestStore(cfg)
}

func TestConfig_configureProviders(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, 1, cfg.Providers.Len())

	// We want to make sure that we keep the configured API key as a placeholder
	pc, _ := cfg.Providers.Get("openai")
	require.Equal(t, "$OPENAI_API_KEY", pc.APIKey)
}

func TestConfig_configureProvidersWithOverride(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMap[string, ProviderConfig](),
	}
	cfg.Providers.Set("openai", ProviderConfig{
		APIKey:  "xyz",
		BaseURL: "https://api.openai.com/v2",
		Models: []catwalk.Model{
			{
				ID:   "test-model",
				Name: "Updated",
			},
			{
				ID: "another-model",
			},
		},
	})
	cfg.setDefaults("/tmp", "")

	env := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, 1, cfg.Providers.Len())

	// We want to make sure that we keep the configured API key as a placeholder
	pc, _ := cfg.Providers.Get("openai")
	require.Equal(t, "xyz", pc.APIKey)
	require.Equal(t, "https://api.openai.com/v2", pc.BaseURL)
	require.Len(t, pc.Models, 2)
	require.Equal(t, "Updated", pc.Models[0].Name)
}

// TestConfig_configureProviders_PreservesLocalCLICustomFields is a regression
// test for a bug where the built-in "local-cli" provider entry was rebuilt
// from a bare literal on every configureProviders call (called on both
// initial Load() and every ReloadFromDisk()), silently discarding any
// user-set field — peak_hours, disable, a custom display name — because only
// ID/Type/Models are meant to be auto-derived from the detected CLI specs.
// Found via a live test of the peak-hours mid-turn cross-process reload
// feature: a peak_hours window set on local-cli never took effect because it
// was wiped back to nil on the very next config load.
func TestConfig_configureProviders_PreservesLocalCLICustomFields(t *testing.T) {
	origAvailable := cliprovider.AvailableFunc
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec {
		return []cliprovider.CLISpec{{ModelID: "cli-claude-sonnet", ModelName: "Claude Sonnet"}}
	}
	t.Cleanup(func() { cliprovider.AvailableFunc = origAvailable })

	cfg := &Config{
		Providers: csync.NewMap[string, ProviderConfig](),
	}
	peakHours := &PeakHoursWindow{Start: "09:00", End: "18:00"}
	cfg.Providers.Set(cliprovider.ProviderID, ProviderConfig{
		Name:      "My Local CLI",
		PeakHours: peakHours,
		Disable:   true,
	})
	cfg.setDefaults("/tmp", "")

	e := env.NewFromMap(map[string]string{})
	resolver := NewShellVariableResolver(e)
	err := cfg.configureProviders(context.Background(), testStore(cfg), e, resolver, []catwalk.Provider{})
	require.NoError(t, err)

	pc, ok := cfg.Providers.Get(cliprovider.ProviderID)
	require.True(t, ok)
	require.Equal(t, "My Local CLI", pc.Name, "custom display name must survive re-synthesis")
	require.True(t, pc.Disable, "disable flag must survive re-synthesis")
	require.NotNil(t, pc.PeakHours, "peak_hours must survive re-synthesis")
	require.Equal(t, "09:00", pc.PeakHours.Start)
	require.Equal(t, "18:00", pc.PeakHours.End)
	// The auto-derived fields still refresh to the newly detected specs.
	require.Equal(t, cliprovider.ProviderID, pc.ID)
	require.EqualValues(t, cliprovider.ProviderType, pc.Type)
	require.Len(t, pc.Models, 1)
	require.Equal(t, "cli-claude-sonnet", pc.Models[0].ID)
}

func TestConfig_configureProvidersWithNewProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"custom": {
				APIKey:  "xyz",
				BaseURL: "https://api.someendpoint.com/v2",
				Models: []catwalk.Model{
					{
						ID: "test-model",
					},
				},
			},
		}),
	}
	cfg.setDefaults("/tmp", "")
	env := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	// Should be to because of the env variable
	require.Equal(t, cfg.Providers.Len(), 2)

	// We want to make sure that we keep the configured API key as a placeholder
	pc, _ := cfg.Providers.Get("custom")
	require.Equal(t, "xyz", pc.APIKey)
	// Make sure we set the ID correctly
	require.Equal(t, "custom", pc.ID)
	require.Equal(t, "https://api.someendpoint.com/v2", pc.BaseURL)
	require.Len(t, pc.Models, 1)

	_, ok := cfg.Providers.Get("openai")
	require.True(t, ok, "OpenAI provider should still be present")
}

func TestConfig_configureProvidersBedrockWithCredentials(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderBedrock,
			APIKey:      "",
			APIEndpoint: "",
			Models: []catwalk.Model{{
				ID: "anthropic.claude-sonnet-4-20250514-v1:0",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := env.NewFromMap(map[string]string{
		"AWS_ACCESS_KEY_ID":     "test-key-id",
		"AWS_SECRET_ACCESS_KEY": "test-secret-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, cfg.Providers.Len(), 1)

	bedrockProvider, ok := cfg.Providers.Get("bedrock")
	require.True(t, ok, "Bedrock provider should be present")
	require.Len(t, bedrockProvider.Models, 1)
	require.Equal(t, "anthropic.claude-sonnet-4-20250514-v1:0", bedrockProvider.Models[0].ID)
}

func TestConfig_configureProvidersBedrockWithoutCredentials(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderBedrock,
			APIKey:      "",
			APIEndpoint: "",
			Models: []catwalk.Model{{
				ID: "anthropic.claude-sonnet-4-20250514-v1:0",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := env.NewFromMap(map[string]string{})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	// Provider should not be configured without credentials
	require.Equal(t, cfg.Providers.Len(), 0)
}

func TestConfig_configureProvidersVertexAIWithCredentials(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderVertexAI,
			APIKey:      "",
			APIEndpoint: "",
			Models: []catwalk.Model{{
				ID: "gemini-pro",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := env.NewFromMap(map[string]string{
		"VERTEXAI_PROJECT":  "test-project",
		"VERTEXAI_LOCATION": "us-central1",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, cfg.Providers.Len(), 1)

	vertexProvider, ok := cfg.Providers.Get("vertexai")
	require.True(t, ok, "VertexAI provider should be present")
	require.Len(t, vertexProvider.Models, 1)
	require.Equal(t, "gemini-pro", vertexProvider.Models[0].ID)
	require.Equal(t, "test-project", vertexProvider.ExtraParams["project"])
	require.Equal(t, "us-central1", vertexProvider.ExtraParams["location"])
}

func TestConfig_configureProvidersVertexAIWithoutCredentials(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderVertexAI,
			APIKey:      "",
			APIEndpoint: "",
			Models: []catwalk.Model{{
				ID: "gemini-pro",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := env.NewFromMap(map[string]string{
		"GOOGLE_GENAI_USE_VERTEXAI": "false",
		"GOOGLE_CLOUD_PROJECT":      "test-project",
		"GOOGLE_CLOUD_LOCATION":     "us-central1",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	// Provider should not be configured without proper credentials
	require.Equal(t, cfg.Providers.Len(), 0)
}

func TestConfig_configureProvidersVertexAIMissingProject(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderVertexAI,
			APIKey:      "",
			APIEndpoint: "",
			Models: []catwalk.Model{{
				ID: "gemini-pro",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := env.NewFromMap(map[string]string{
		"GOOGLE_GENAI_USE_VERTEXAI": "true",
		"GOOGLE_CLOUD_LOCATION":     "us-central1",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	// Provider should not be configured without project
	require.Equal(t, cfg.Providers.Len(), 0)
}

func TestConfig_configureProvidersSetProviderID(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")
	env := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, cfg.Providers.Len(), 1)

	// Provider ID should be set
	pc, _ := cfg.Providers.Get("openai")
	require.Equal(t, "openai", pc.ID)
}

func TestConfig_EnabledProviders(t *testing.T) {
	t.Run("all providers enabled", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					ID:      "openai",
					APIKey:  "key1",
					Disable: false,
				},
				"anthropic": {
					ID:      "anthropic",
					APIKey:  "key2",
					Disable: false,
				},
			}),
		}

		enabled := cfg.EnabledProviders()
		require.Len(t, enabled, 2)
	})

	t.Run("some providers disabled", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					ID:      "openai",
					APIKey:  "key1",
					Disable: false,
				},
				"anthropic": {
					ID:      "anthropic",
					APIKey:  "key2",
					Disable: true,
				},
			}),
		}

		enabled := cfg.EnabledProviders()
		require.Len(t, enabled, 1)
		require.Equal(t, "openai", enabled[0].ID)
	})

	t.Run("empty providers map", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap[string, ProviderConfig](),
		}

		enabled := cfg.EnabledProviders()
		require.Len(t, enabled, 0)
	})
}

func TestConfig_IsConfigured(t *testing.T) {
	t.Run("returns true when at least one provider is enabled", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					ID:      "openai",
					APIKey:  "key1",
					Disable: false,
				},
			}),
		}

		require.True(t, cfg.IsConfigured())
	})

	t.Run("returns false when no providers are configured", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMap[string, ProviderConfig](),
		}

		require.False(t, cfg.IsConfigured())
	})

	t.Run("returns false when all providers are disabled", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					ID:      "openai",
					APIKey:  "key1",
					Disable: true,
				},
				"anthropic": {
					ID:      "anthropic",
					APIKey:  "key2",
					Disable: true,
				},
			}),
		}

		require.False(t, cfg.IsConfigured())
	})
}

func TestConfig_setupAgentsWithNoDisabledTools(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)
	assert.Equal(t, allToolNames(), coderAgent.AllowedTools)

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	assert.Equal(t, []string{"glob", "grep", "ls", "sourcegraph", "view"}, taskAgent.AllowedTools)
}

func TestConfig_setupAgentsWithDisabledTools(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{
				"edit",
				"download",
				"grep",
			},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)
	assert.NotContains(t, coderAgent.AllowedTools, "edit")
	assert.NotContains(t, coderAgent.AllowedTools, "download")
	assert.NotContains(t, coderAgent.AllowedTools, "grep")

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	assert.Equal(t, []string{"glob", "ls", "sourcegraph", "view"}, taskAgent.AllowedTools)
}

func TestConfig_setupAgentsWithEveryReadOnlyToolDisabled(t *testing.T) {
	cfg := &Config{
		Options: &Options{
			DisabledTools: []string{
				"glob",
				"grep",
				"ls",
				"sourcegraph",
				"view",
			},
		},
	}

	cfg.SetupAgents()
	coderAgent, ok := cfg.Agents[AgentCoder]
	require.True(t, ok)
	assert.NotContains(t, coderAgent.AllowedTools, "glob")
	assert.NotContains(t, coderAgent.AllowedTools, "view")

	taskAgent, ok := cfg.Agents[AgentTask]
	require.True(t, ok)
	assert.Len(t, taskAgent.AllowedTools, 0)
}

func TestConfig_configureProvidersWithDisabledProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models: []catwalk.Model{{
				ID: "test-model",
			}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"openai": {
				Disable: true,
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	env := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)

	require.Equal(t, cfg.Providers.Len(), 1)
	prov, exists := cfg.Providers.Get("openai")
	require.True(t, exists)
	require.True(t, prov.Disable)
}
