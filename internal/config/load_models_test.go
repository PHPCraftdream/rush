// Selected-model tests: defaultModelSelection fallbacks and provider
// precedence, and configureSelectedModels override and persistence
// behaviour.

package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/stretchr/testify/require"
)

func TestConfig_defaultModelSelection(t *testing.T) {
	t.Run("default behavior uses the default models for given provider", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		large, small, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "large-model", large.Model)
		require.Equal(t, "openai", large.Provider)
		require.Equal(t, int64(1000), large.MaxTokens)
		require.Equal(t, "small-model", small.Model)
		require.Equal(t, "openai", small.Provider)
		require.Equal(t, int64(500), small.MaxTokens)
	})
	t.Run("should error if no providers configured", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "$MISSING_KEY",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		_, _, err = cfg.defaultModelSelection(knownProviders)
		require.Error(t, err)
	})
	t.Run("should not error if default model is missing but provider has any models", func(t *testing.T) {
		// Ported from upstream ffaeec19 (#3066): falls back to the first
		// available model rather than failing startup, since a model can
		// disappear from a provider's catalog at any time.
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "not-large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		_, _, err = cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
	})

	t.Run("should configure the default models with a custom provider", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "$MISSING", // will not be included in the config
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "not-large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models: []catwalk.Model{
						{
							ID:               "model",
							DefaultMaxTokens: 600,
						},
					},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		large, small, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "model", large.Model)
		require.Equal(t, "custom", large.Provider)
		require.Equal(t, int64(600), large.MaxTokens)
		require.Equal(t, "model", small.Model)
		require.Equal(t, "custom", small.Provider)
		require.Equal(t, int64(600), small.MaxTokens)
	})

	t.Run("should fail if no model configured", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "$MISSING", // will not be included in the config
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "not-large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models:  []catwalk.Model{},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		_, _, err = cfg.defaultModelSelection(knownProviders)
		require.Error(t, err)
	})
	t.Run("should use the default provider first", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "set",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Models: []catwalk.Model{
						{
							ID:               "large-model",
							DefaultMaxTokens: 1000,
						},
					},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)
		large, small, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "large-model", large.Model)
		require.Equal(t, "openai", large.Provider)
		require.Equal(t, int64(1000), large.MaxTokens)
		require.Equal(t, "small-model", small.Model)
		require.Equal(t, "openai", small.Provider)
		require.Equal(t, int64(500), small.MaxTokens)
	})
}

func TestConfig_configureSelectedModels(t *testing.T) {
	t.Run("reload mode should not persist fallback defaults", func(t *testing.T) {
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "crush.json")
		require.NoError(t, os.WriteFile(globalPath, []byte(`{"models":{"large":{"provider":"ghost","model":"missing"}}}`), 0o600))

		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{ID: "large-model", DefaultMaxTokens: 1000},
					{ID: "small-model", DefaultMaxTokens: 500},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				SelectedModelTypeLarge: {Provider: "ghost", Model: "missing"},
			},
		}
		cfg.setDefaults(dir, "")
		store := newTestConfigStore(testStoreOpts{config: cfg, globalDataPath: globalPath})
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), store, env, resolver, knownProviders)
		require.NoError(t, err)

		err = configureSelectedModels(store, cfg, knownProviders, false)
		require.NoError(t, err)

		// In-memory falls back to default.
		require.Equal(t, "openai", cfg.Models[SelectedModelTypeLarge].Provider)
		require.Equal(t, "large-model", cfg.Models[SelectedModelTypeLarge].Model)

		// Disk remains unchanged in reload mode.
		data, readErr := os.ReadFile(globalPath)
		require.NoError(t, readErr)
		require.Contains(t, string(data), `"provider":"ghost"`)
		require.Contains(t, string(data), `"model":"missing"`)
	})
	t.Run("should override defaults", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "larger-model",
						DefaultMaxTokens: 2000,
					},
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				"large": {
					Model: "larger-model",
				},
			},
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		err = configureSelectedModels(testStore(cfg), cfg, knownProviders, true)
		require.NoError(t, err)
		large := cfg.Models[SelectedModelTypeLarge]
		small := cfg.Models[SelectedModelTypeSmall]
		require.Equal(t, "larger-model", large.Model)
		require.Equal(t, "openai", large.Provider)
		require.Equal(t, int64(2000), large.MaxTokens)
		require.Equal(t, "small-model", small.Model)
		require.Equal(t, "openai", small.Provider)
		require.Equal(t, int64(500), small.MaxTokens)
	})
	t.Run("should be possible to use multiple providers", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
			{
				ID:                  "anthropic",
				APIKey:              "abc",
				DefaultLargeModelID: "a-large-model",
				DefaultSmallModelID: "a-small-model",
				Models: []catwalk.Model{
					{
						ID:               "a-large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "a-small-model",
						DefaultMaxTokens: 200,
					},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				"small": {
					Model:     "a-small-model",
					Provider:  "anthropic",
					MaxTokens: 300,
				},
			},
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		err = configureSelectedModels(testStore(cfg), cfg, knownProviders, true)
		require.NoError(t, err)
		large := cfg.Models[SelectedModelTypeLarge]
		small := cfg.Models[SelectedModelTypeSmall]
		require.Equal(t, "large-model", large.Model)
		require.Equal(t, "openai", large.Provider)
		require.Equal(t, int64(1000), large.MaxTokens)
		require.Equal(t, "a-small-model", small.Model)
		require.Equal(t, "anthropic", small.Provider)
		require.Equal(t, int64(300), small.MaxTokens)
	})

	t.Run("should override the max tokens only", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				"large": {
					MaxTokens: 100,
				},
			},
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		err = configureSelectedModels(testStore(cfg), cfg, knownProviders, true)
		require.NoError(t, err)
		large := cfg.Models[SelectedModelTypeLarge]
		require.Equal(t, "large-model", large.Model)
		require.Equal(t, "openai", large.Provider)
		require.Equal(t, int64(100), large.MaxTokens)
	})

	t.Run("should not leak reasoning effort from a previous provider", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "anthropic",
				APIKey:              "abc",
				DefaultLargeModelID: "a-large-model",
				DefaultSmallModelID: "a-small-model",
				Models: []catwalk.Model{
					{
						ID:                     "a-large-model",
						DefaultMaxTokens:       1000,
						DefaultReasoningEffort: "high",
					},
					{
						ID:               "a-small-model",
						DefaultMaxTokens: 200,
					},
				},
			},
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "large-model",
				DefaultSmallModelID: "small-model",
				Models: []catwalk.Model{
					{
						ID:               "large-model",
						DefaultMaxTokens: 1000,
						// No default reasoning effort for this provider/model.
					},
					{
						ID:               "small-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		// Simulate a stale user preference carrying the reasoning effort
		// from a previously selected provider (anthropic's "high") while
		// switching to a provider/model that defines no reasoning effort.
		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				"large": {
					Provider:        "openai",
					Model:           "large-model",
					ReasoningEffort: "",
				},
				"small": {
					Provider:        "openai",
					Model:           "small-model",
					ReasoningEffort: "",
				},
			},
		}
		cfg.setDefaults("/tmp", "")
		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		err = configureSelectedModels(testStore(cfg), cfg, knownProviders, true)
		require.NoError(t, err)
		large := cfg.Models[SelectedModelTypeLarge]
		small := cfg.Models[SelectedModelTypeSmall]
		require.Equal(t, "openai", large.Provider)
		require.Empty(t, large.ReasoningEffort, "large model must not inherit stale reasoning effort")
		require.Equal(t, "openai", small.Provider)
		require.Empty(t, small.ReasoningEffort, "small model must not inherit stale reasoning effort")
	})
}
