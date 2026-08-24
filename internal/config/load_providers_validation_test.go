// Provider validation tests: custom provider rules (base URL, models,
// type), credential requirements that drop providers, and the
// disable_default_providers strict mode.

package config

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/stretchr/testify/require"
)

func TestConfig_configureProvidersCustomProviderValidation(t *testing.T) {
	t.Run("custom provider with missing API key is allowed, but not known providers", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					BaseURL: "https://api.custom.com/v1",
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
				"openai": {
					APIKey: "$MISSING",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 1)
		_, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
	})

	t.Run("custom provider with missing BaseURL is removed", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey: "test-key",
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})

	t.Run("custom provider with no models is removed", func(t *testing.T) {
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
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})

	t.Run("custom provider with unsupported type is removed", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Type:    "unsupported",
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})

	t.Run("valid custom provider is kept and ID is set", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Type:    catwalk.TypeOpenAI,
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 1)
		customProvider, exists := cfg.Providers.Get("custom")
		require.True(t, exists)
		require.Equal(t, "custom", customProvider.ID)
		require.Equal(t, "test-key", customProvider.APIKey)
		require.Equal(t, "https://api.custom.com/v1", customProvider.BaseURL)
	})

	t.Run("custom anthropic provider is supported", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom-anthropic": {
					APIKey:  "test-key",
					BaseURL: "https://api.anthropic.com/v1",
					Type:    catwalk.TypeAnthropic,
					Models: []catwalk.Model{{
						ID: "claude-3-sonnet",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 1)
		customProvider, exists := cfg.Providers.Get("custom-anthropic")
		require.True(t, exists)
		require.Equal(t, "custom-anthropic", customProvider.ID)
		require.Equal(t, "test-key", customProvider.APIKey)
		require.Equal(t, "https://api.anthropic.com/v1", customProvider.BaseURL)
		require.Equal(t, catwalk.TypeAnthropic, customProvider.Type)
	})

	t.Run("disabled custom provider is removed", func(t *testing.T) {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					APIKey:  "test-key",
					BaseURL: "https://api.custom.com/v1",
					Type:    catwalk.TypeOpenAI,
					Disable: true,
					Models: []catwalk.Model{{
						ID: "test-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("custom")
		require.False(t, exists)
	})
}

func TestConfig_configureProvidersEnhancedCredentialValidation(t *testing.T) {
	t.Run("VertexAI provider removed when credentials missing with existing config", func(t *testing.T) {
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

		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"vertexai": {
					BaseURL: "custom-url",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{
			"GOOGLE_GENAI_USE_VERTEXAI": "false",
		})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("vertexai")
		require.False(t, exists)
	})

	t.Run("Bedrock provider removed when AWS credentials missing with existing config", func(t *testing.T) {
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

		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"bedrock": {
					BaseURL: "custom-url",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("bedrock")
		require.False(t, exists)
	})

	t.Run("provider removed when API key missing with existing config", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$MISSING_API_KEY",
				APIEndpoint: "https://api.openai.com/v1",
				Models: []catwalk.Model{{
					ID: "test-model",
				}},
			},
		}

		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					BaseURL: "custom-url",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		require.Equal(t, cfg.Providers.Len(), 0)
		_, exists := cfg.Providers.Get("openai")
		require.False(t, exists)
	})

	t.Run("known provider should still be added if the endpoint is missing the client will use default endpoints", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$OPENAI_API_KEY",
				APIEndpoint: "$MISSING_ENDPOINT",
				Models: []catwalk.Model{{
					ID: "test-model",
				}},
			},
		}

		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					APIKey: "test-key",
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
		_, exists := cfg.Providers.Get("openai")
		require.True(t, exists)
	})
}

func TestConfig_configureProvidersDisableDefaultProviders(t *testing.T) {
	t.Run("when enabled, ignores all default providers and requires full specification", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$OPENAI_API_KEY",
				APIEndpoint: "https://api.openai.com/v1",
				Models: []catwalk.Model{{
					ID: "gpt-4",
				}},
			},
		}

		// User references openai but doesn't fully specify it (no base_url, no
		// models). This should be rejected because disable_default_providers
		// treats all providers as custom.
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					APIKey: "$OPENAI_API_KEY",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{
			"OPENAI_API_KEY": "test-key",
		})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.ErrorContains(t, err, "no custom providers")

		// openai should NOT be present because it lacks base_url and models.
		require.Equal(t, 0, cfg.Providers.Len())
		_, exists := cfg.Providers.Get("openai")
		require.False(t, exists, "openai should not be present without full specification")
	})

	t.Run("when enabled, fully specified providers work", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$OPENAI_API_KEY",
				APIEndpoint: "https://api.openai.com/v1",
				Models: []catwalk.Model{{
					ID: "gpt-4",
				}},
			},
		}

		// User fully specifies their provider.
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"my-llm": {
					APIKey:  "$MY_API_KEY",
					BaseURL: "https://my-llm.example.com/v1",
					Models: []catwalk.Model{{
						ID: "my-model",
					}},
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{
			"MY_API_KEY":     "test-key",
			"OPENAI_API_KEY": "test-key",
		})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		// Only fully specified provider should be present.
		require.Equal(t, 1, cfg.Providers.Len())
		provider, exists := cfg.Providers.Get("my-llm")
		require.True(t, exists, "my-llm should be present")
		require.Equal(t, "https://my-llm.example.com/v1", provider.BaseURL)
		require.Len(t, provider.Models, 1)

		// Default openai should NOT be present.
		_, exists = cfg.Providers.Get("openai")
		require.False(t, exists, "openai should not be present")
	})

	t.Run("when disabled, includes all known providers with valid credentials", func(t *testing.T) {
		knownProviders := []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "$OPENAI_API_KEY",
				APIEndpoint: "https://api.openai.com/v1",
				Models: []catwalk.Model{{
					ID: "gpt-4",
				}},
			},
			{
				ID:          "anthropic",
				APIKey:      "$ANTHROPIC_API_KEY",
				APIEndpoint: "https://api.anthropic.com/v1",
				Models: []catwalk.Model{{
					ID: "claude-3",
				}},
			},
		}

		// User only configures openai, both API keys are available, but option
		// is disabled.
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: false,
			},
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"openai": {
					APIKey: "$OPENAI_API_KEY",
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{
			"OPENAI_API_KEY":    "test-key",
			"ANTHROPIC_API_KEY": "test-key",
		})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
		require.NoError(t, err)

		// Both providers should be present.
		require.Equal(t, 2, cfg.Providers.Len())
		_, exists := cfg.Providers.Get("openai")
		require.True(t, exists, "openai should be present")
		_, exists = cfg.Providers.Get("anthropic")
		require.True(t, exists, "anthropic should be present")
	})

	t.Run("when enabled, provider missing models is rejected", func(t *testing.T) {
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"my-llm": {
					APIKey:  "test-key",
					BaseURL: "https://my-llm.example.com/v1",
					Models:  []catwalk.Model{}, // No models.
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.ErrorContains(t, err, "no custom providers")

		// Provider should be rejected for missing models.
		require.Equal(t, 0, cfg.Providers.Len())
	})

	t.Run("when enabled, provider missing base_url is rejected", func(t *testing.T) {
		cfg := &Config{
			Options: &Options{
				DisableDefaultProviders: true,
			},
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"my-llm": {
					APIKey: "test-key",
					Models: []catwalk.Model{{ID: "model"}},
					// No BaseURL.
				},
			}),
		}
		cfg.setDefaults("/tmp", "")

		env := env.NewFromMap(map[string]string{})
		resolver := NewShellVariableResolver(env)
		err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, []catwalk.Provider{})
		require.ErrorContains(t, err, "no custom providers")

		// Provider should be rejected for missing base_url.
		require.Equal(t, 0, cfg.Providers.Len())
	})
}
