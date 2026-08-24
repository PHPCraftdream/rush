// Environment and shell-variable resolution tests during provider
// configuration: HYPER_API_KEY precedence, extra-header resolution
// ($(cmd) and unset variables), and unset or failing API keys and
// endpoints skipping providers.

package config

import (
	"context"
	"os"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/stretchr/testify/require"
)

func TestConfig_configureProviders_HyperAPIKeyFromEnv(t *testing.T) {
	// Test that HYPER_API_KEY environment variable works without config
	knownProviders := []catwalk.Provider{
		{
			ID:                  "hyper",
			APIKey:              "", // No API key in provider definition
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
	env := env.NewFromMap(map[string]string{
		"HYPER_API_KEY": "env-api-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, 1, cfg.Providers.Len())

	// Verify Hyper provider is configured with the env var API key
	pc, ok := cfg.Providers.Get("hyper")
	require.True(t, ok, "Hyper provider should be configured")
	require.Equal(t, "env-api-key", pc.APIKey)
	require.Equal(t, "env-api-key", pc.APIKeyTemplate)
}

func TestConfig_configureProviders_HyperAPIKeyFromConfigOverrides(t *testing.T) {
	// Test that config API key takes precedence when HYPER_API_KEY is also set
	knownProviders := []catwalk.Provider{
		{
			ID:                  "hyper",
			APIKey:              "provider-api-key",
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

	// User has Hyper configured with an API key
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"hyper": {
				APIKey: "config-api-key",
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	// But they also have HYPER_API_KEY set - env var should take precedence
	env := env.NewFromMap(map[string]string{
		"HYPER_API_KEY": "env-api-key",
	})
	resolver := NewShellVariableResolver(env)
	err := cfg.configureProviders(context.Background(), testStore(cfg), env, resolver, knownProviders)
	require.NoError(t, err)
	require.Equal(t, 1, cfg.Providers.Len())

	// Verify env var takes precedence (as per requirements)
	pc, ok := cfg.Providers.Get("hyper")
	require.True(t, ok, "Hyper provider should be configured")
	require.Equal(t, "env-api-key", pc.APIKey)
}

// TestConfig_configureProviders_ProviderHeaderResolveError verifies
// that a failing $(cmd) in a provider header fails the provider load
// with a clear message that names the offending header. Provider
// headers share the MCP error contract.
func TestConfig_configureProviders_ProviderHeaderResolveError(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"openai": {
				ExtraHeaders: map[string]string{
					// Failing $(...) — inner command exits 1. Must
					// propagate as an error, not a silent truncation.
					"X-Broken": "$(false)",
				},
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
		"PATH":           os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.Error(t, err, "failing $(cmd) in a header must fail the provider load")
	require.Contains(t, err.Error(), "X-Broken", "error must name the offending header")
}

// TestConfig_configureProviders_CatwalkDefaultWithUnsetVarLoads
// verifies that a Catwalk-style default header like
// "OpenAI-Organization": "$OPENAI_ORG_ID" loads cleanly under lenient
// nounset (unset → "" → header dropped), and does not fail the load
// or leave the literal template on the wire.
func TestConfig_configureProviders_CatwalkDefaultWithUnsetVarLoads(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
			DefaultHeaders: map[string]string{
				"OpenAI-Organization": "$OPENAI_ORG_ID",
			},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
		"PATH":           os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err, "optional env-gated header must not fail the load")

	pc, ok := cfg.Providers.Get("openai")
	require.True(t, ok, "openai provider must still be configured")
	_, present := pc.ExtraHeaders["OpenAI-Organization"]
	require.False(t, present, "header whose value resolves to empty must be absent")
}

// TestConfig_configureProviders_LiteralEmptyHeaderDropped pins design
// decision #18 for the literal case: a user-authored
// "X-Custom": "" in extra_headers is absent from the resolved map.
// Applies to both known- and custom-provider paths; this test
// exercises the custom-provider loop.
func TestConfig_configureProviders_LiteralEmptyHeaderDropped(t *testing.T) {
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"my-llm": {
				APIKey:  "test-key",
				BaseURL: "https://my-llm.example.com/v1",
				Type:    catwalk.TypeOpenAI,
				Models:  []catwalk.Model{{ID: "m"}},
				ExtraHeaders: map[string]string{
					"X-Custom": "",
					"X-Kept":   "present",
				},
			},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"PATH": os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, []catwalk.Provider{})
	require.NoError(t, err)

	pc, ok := cfg.Providers.Get("my-llm")
	require.True(t, ok)
	_, present := pc.ExtraHeaders["X-Custom"]
	require.False(t, present, "literal empty-string header must be dropped")
	require.Equal(t, "present", pc.ExtraHeaders["X-Kept"])
}

// TestConfig_configureProviders_EchoEmptyHeaderDropped pins design
// decision #18 for the non-failing empty case: $(echo) exits 0 with
// empty output, resolves cleanly to "", and must be dropped the same
// way an unset bare $VAR is. Exercises the known-provider loop.
func TestConfig_configureProviders_EchoEmptyHeaderDropped(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
			DefaultHeaders: map[string]string{
				"X-Empty": "$(echo)",
				"X-Kept":  "present",
			},
		},
	}

	cfg := &Config{}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY": "test-key",
		"PATH":           os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err)

	pc, ok := cfg.Providers.Get("openai")
	require.True(t, ok)
	_, present := pc.ExtraHeaders["X-Empty"]
	require.False(t, present, "$(echo) → empty → header must be dropped")
	require.Equal(t, "present", pc.ExtraHeaders["X-Kept"])
}

// TestConfig_configureProviders_UnsetAPIKeySkipsProvider verifies that
// under the lenient-nounset shell resolver, $UNSET_API_KEY expands to
// ("", nil) rather than ("", err), and the existing
// `v == "" || err != nil` skip path at load.go:331 still drops the
// provider. The slog.Warn line is emitted on the same
// path but is not asserted here — internal/config/load_test.go's
// TestMain replaces the default slog handler with an io.Discard
// writer, so capturing that log line would require mid-test handler
// swapping and a sync.Mutex dance that adds more flake surface than
// signal. The observable outcome (provider absent from the map) is
// what downstream code — model picker, agent wiring — actually reads,
// so that's what we pin.
func TestConfig_configureProviders_UnsetAPIKeySkipsProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$SOMETHING_UNSET",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
		},
	}

	// Existing user config for this known provider so the load.go:332
	// `if configExists` branch fires and actually calls Providers.Del.
	// Without it the provider was never in the map to begin with and
	// the test would pass trivially.
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"openai": {BaseURL: "custom-url"},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"PATH": os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err, "skip path must not surface as a load error")

	require.Equal(t, 0, cfg.Providers.Len(), "provider with unset API key must be skipped")
	_, exists := cfg.Providers.Get("openai")
	require.False(t, exists)
}

// TestConfig_configureProviders_FailingAPIKeyCmdSkipsProvider pins
// that the two failure modes for APIKey — ("", nil) from an unset var
// under lenient nounset and ("", err) from a failing $(cmd) — are
// equivalent for the skip outcome at load.go:331. The `v == "" ||
// err != nil` check fires on either branch; this test locks in that
// equivalence so a future refactor that splits the check into two
// paths doesn't accidentally start propagating $(false) as a load
// error while keeping unset-var as a silent skip (or vice versa).
func TestConfig_configureProviders_FailingAPIKeyCmdSkipsProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$(false)",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"openai": {BaseURL: "custom-url"},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"PATH": os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err, "failing $(cmd) in API key must skip provider, not fail load")

	require.Equal(t, 0, cfg.Providers.Len(), "provider with failing $(cmd) API key must be skipped")
	_, exists := cfg.Providers.Get("openai")
	require.False(t, exists)
}

// TestConfig_configureProviders_UnsetAzureEndpointSkipsProvider pins
// the same contract on the Azure path at load.go:287 — APIEndpoint is
// the field that gates Azure and goes through the same
// `v == "" || err != nil` skip check. Covered here so both branches
// of the shared skip pattern (APIKey default path and APIEndpoint
// Azure path) are tested; a future refactor that unifies them can
// rely on these two tests to catch drift.
func TestConfig_configureProviders_UnsetAzureEndpointSkipsProvider(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          catwalk.InferenceProviderAzure,
			APIKey:      "test-key",
			APIEndpoint: "$UNSET_AZURE_ENDPOINT",
			Models:      []catwalk.Model{{ID: "test-model"}},
		},
	}

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"azure": {BaseURL: ""},
		}),
	}
	cfg.setDefaults("/tmp", "")

	testEnv := env.NewFromMap(map[string]string{
		"PATH": os.Getenv("PATH"),
	})
	resolver := NewShellVariableResolver(testEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), testEnv, resolver, knownProviders)
	require.NoError(t, err)

	require.Equal(t, 0, cfg.Providers.Len(), "azure provider with unset endpoint must be skipped")
	_, exists := cfg.Providers.Get("azure")
	require.False(t, exists)
}
