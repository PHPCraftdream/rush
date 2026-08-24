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
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/stretchr/testify/require"
)

func TestConfig_defaultModelSelection(t *testing.T) {
	t.Run("default behavior uses the default models for given provider", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "smart-model",
				DefaultSmallModelID: "fast-model",
				Models: []catwalk.Model{
					{
						ID:               "smart-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "fast-model",
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

		smart, fast, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "smart-model", smart.Model)
		require.Equal(t, "openai", smart.Provider)
		require.Equal(t, int64(1000), smart.MaxTokens)
		require.Equal(t, "fast-model", fast.Model)
		require.Equal(t, "openai", fast.Provider)
		require.Equal(t, int64(500), fast.MaxTokens)
	})
	t.Run("should error if no providers configured", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "$MISSING_KEY",
				DefaultLargeModelID: "smart-model",
				DefaultSmallModelID: "fast-model",
				Models: []catwalk.Model{
					{
						ID:               "smart-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "fast-model",
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
				DefaultLargeModelID: "smart-model",
				DefaultSmallModelID: "fast-model",
				Models: []catwalk.Model{
					{
						ID:               "not-smart-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "fast-model",
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
				DefaultLargeModelID: "smart-model",
				DefaultSmallModelID: "fast-model",
				Models: []catwalk.Model{
					{
						ID:               "not-smart-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "fast-model",
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
		smart, fast, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "model", smart.Model)
		require.Equal(t, "custom", smart.Provider)
		require.Equal(t, int64(600), smart.MaxTokens)
		require.Equal(t, "model", fast.Model)
		require.Equal(t, "custom", fast.Provider)
		require.Equal(t, int64(600), fast.MaxTokens)
	})

	t.Run("should fail if no model configured", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "$MISSING", // will not be included in the config
				DefaultLargeModelID: "smart-model",
				DefaultSmallModelID: "fast-model",
				Models: []catwalk.Model{
					{
						ID:               "not-smart-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "fast-model",
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
				DefaultLargeModelID: "smart-model",
				DefaultSmallModelID: "fast-model",
				Models: []catwalk.Model{
					{
						ID:               "smart-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "fast-model",
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
							ID:               "smart-model",
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
		smart, fast, err := cfg.defaultModelSelection(knownProviders)
		require.NoError(t, err)
		require.Equal(t, "smart-model", smart.Model)
		require.Equal(t, "openai", smart.Provider)
		require.Equal(t, int64(1000), smart.MaxTokens)
		require.Equal(t, "fast-model", fast.Model)
		require.Equal(t, "openai", fast.Provider)
		require.Equal(t, int64(500), fast.MaxTokens)
	})
}

func TestConfig_configureSelectedModels(t *testing.T) {
	t.Run("reload mode should not persist fallback defaults", func(t *testing.T) {
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "rush.json")
		require.NoError(t, os.WriteFile(globalPath, []byte(`{"models":{"smart":{"provider":"ghost","model":"missing"}}}`), 0o600))

		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "smart-model",
				DefaultSmallModelID: "fast-model",
				Models: []catwalk.Model{
					{ID: "smart-model", DefaultMaxTokens: 1000},
					{ID: "fast-model", DefaultMaxTokens: 500},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				SelectedModelTypeSmart: {Provider: "ghost", Model: "missing"},
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
		require.Equal(t, "openai", cfg.Models[SelectedModelTypeSmart].Provider)
		require.Equal(t, "smart-model", cfg.Models[SelectedModelTypeSmart].Model)

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
				DefaultLargeModelID: "smart-model",
				DefaultSmallModelID: "fast-model",
				Models: []catwalk.Model{
					{
						ID:               "larger-model",
						DefaultMaxTokens: 2000,
					},
					{
						ID:               "smart-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "fast-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				"smart": {
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
		smart := cfg.Models[SelectedModelTypeSmart]
		fast := cfg.Models[SelectedModelTypeFast]
		require.Equal(t, "larger-model", smart.Model)
		require.Equal(t, "openai", smart.Provider)
		require.Equal(t, int64(2000), smart.MaxTokens)
		require.Equal(t, "fast-model", fast.Model)
		require.Equal(t, "openai", fast.Provider)
		require.Equal(t, int64(500), fast.MaxTokens)
	})
	t.Run("should be possible to use multiple providers", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "smart-model",
				DefaultSmallModelID: "fast-model",
				Models: []catwalk.Model{
					{
						ID:               "smart-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "fast-model",
						DefaultMaxTokens: 500,
					},
				},
			},
			{
				ID:                  "anthropic",
				APIKey:              "abc",
				DefaultLargeModelID: "a-smart-model",
				DefaultSmallModelID: "a-fast-model",
				Models: []catwalk.Model{
					{
						ID:               "a-smart-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "a-fast-model",
						DefaultMaxTokens: 200,
					},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				"fast": {
					Model:     "a-fast-model",
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
		smart := cfg.Models[SelectedModelTypeSmart]
		fast := cfg.Models[SelectedModelTypeFast]
		require.Equal(t, "smart-model", smart.Model)
		require.Equal(t, "openai", smart.Provider)
		require.Equal(t, int64(1000), smart.MaxTokens)
		require.Equal(t, "a-fast-model", fast.Model)
		require.Equal(t, "anthropic", fast.Provider)
		require.Equal(t, int64(300), fast.MaxTokens)
	})

	t.Run("should override the max tokens only", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "smart-model",
				DefaultSmallModelID: "fast-model",
				Models: []catwalk.Model{
					{
						ID:               "smart-model",
						DefaultMaxTokens: 1000,
					},
					{
						ID:               "fast-model",
						DefaultMaxTokens: 500,
					},
				},
			},
		}

		cfg := &Config{
			Models: map[SelectedModelType]SelectedModel{
				"smart": {
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
		smart := cfg.Models[SelectedModelTypeSmart]
		require.Equal(t, "smart-model", smart.Model)
		require.Equal(t, "openai", smart.Provider)
		require.Equal(t, int64(100), smart.MaxTokens)
	})

	t.Run("should not leak reasoning effort from a previous provider", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:                  "anthropic",
				APIKey:              "abc",
				DefaultLargeModelID: "a-smart-model",
				DefaultSmallModelID: "a-fast-model",
				Models: []catwalk.Model{
					{
						ID:                     "a-smart-model",
						DefaultMaxTokens:       1000,
						DefaultReasoningEffort: "high",
					},
					{
						ID:               "a-fast-model",
						DefaultMaxTokens: 200,
					},
				},
			},
			{
				ID:                  "openai",
				APIKey:              "abc",
				DefaultLargeModelID: "smart-model",
				DefaultSmallModelID: "fast-model",
				Models: []catwalk.Model{
					{
						ID:               "smart-model",
						DefaultMaxTokens: 1000,
						// No default reasoning effort for this provider/model.
					},
					{
						ID:               "fast-model",
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
				"smart": {
					Provider:        "openai",
					Model:           "smart-model",
					ReasoningEffort: "",
				},
				"fast": {
					Provider:        "openai",
					Model:           "fast-model",
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
		smart := cfg.Models[SelectedModelTypeSmart]
		fast := cfg.Models[SelectedModelTypeFast]
		require.Equal(t, "openai", smart.Provider)
		require.Empty(t, smart.ReasoningEffort, "smart model must not inherit stale reasoning effort")
		require.Equal(t, "openai", fast.Provider)
		require.Empty(t, fast.ReasoningEffort, "fast model must not inherit stale reasoning effort")
	})
}
