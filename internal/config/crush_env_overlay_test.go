package config

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/stretchr/testify/require"
)

// These tests cover the P1.3 fix: configureProviders used to honour the
// RUSH_X override convention by temporarily os.Setenv-ing the process's
// real environment (PushPopCrushEnv) and restoring it afterwards. That
// mutated global process state — visible to every other goroutine and any
// child process spawned while the override was live — and, because
// os.Getenv cannot distinguish "was unset" from "was empty", could leave a
// variable that was absent before the call permanently set to "" after
// "restoring" it.
//
// The replacement (rushEnvOverlay + env.NewOverlay) computes the same
// RUSH_X -> X mapping as a pure function and threads it explicitly
// through an env.Env value passed to the resolver and to configureProviders'
// own env.Get calls, with no os.Setenv/os.Unsetenv anywhere in the path.

// TestRushEnvOverlay_MapsPrefixToBareName pins the core mapping: a
// RUSH_X entry in the base Env becomes visible as X in the overlay,
// independent of whatever X is set to in the base.
func TestRushEnvOverlay_MapsPrefixToBareName(t *testing.T) {
	base := env.NewFromMap(map[string]string{
		"RUSH_OPENAI_API_KEY": "overridden-key",
		"OPENAI_API_KEY":      "real-key",
		"UNRELATED":           "unrelated-value",
	})

	overlay := rushEnvOverlay(base)
	require.Equal(t, map[string]string{"OPENAI_API_KEY": "overridden-key"}, overlay)
}

// TestConfigureProviders_RushPrefixOverridesAPIKey exercises the
// documented escape hatch end to end: a provider header template
// referencing $OPENAI_API_KEY resolves to the RUSH_-prefixed override
// when present, not the bare env var.
//
// Headers (unlike ProviderConfig.APIKey, which always stores the
// unresolved template as a placeholder — see TestConfig_configureProviders)
// store the RESOLVED value, so they're the observable signal for proving
// the overlay actually took effect during resolution.
func TestConfigureProviders_RushPrefixOverridesAPIKey(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "test-key",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
			DefaultHeaders: map[string]string{
				"X-Probe": "$OPENAI_API_KEY",
			},
		},
	}

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	baseEnv := env.NewFromMap(map[string]string{
		"OPENAI_API_KEY":      "real-key",
		"RUSH_OPENAI_API_KEY": "crush-override-key",
	})
	resolver := NewShellVariableResolver(baseEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), baseEnv, resolver, knownProviders)
	require.NoError(t, err)

	pc, ok := cfg.Providers.Get("openai")
	require.True(t, ok)
	require.Equal(t, "crush-override-key", pc.ExtraHeaders["X-Probe"], "RUSH_ prefix must override the bare env var")
}

// TestConfigureProviders_NoOverlayWhenNoRushPrefix verifies the common
// case (no RUSH_ vars set at all) behaves exactly as before: the plain
// env var resolves normally.
func TestConfigureProviders_NoOverlayWhenNoRushPrefix(t *testing.T) {
	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "test-key",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
			DefaultHeaders: map[string]string{
				"X-Probe": "$OPENAI_API_KEY",
			},
		},
	}

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	baseEnv := env.NewFromMap(map[string]string{"OPENAI_API_KEY": "real-key"})
	resolver := NewShellVariableResolver(baseEnv)

	err := cfg.configureProviders(context.Background(), testStore(cfg), baseEnv, resolver, knownProviders)
	require.NoError(t, err)

	pc, ok := cfg.Providers.Get("openai")
	require.True(t, ok)
	require.Equal(t, "real-key", pc.ExtraHeaders["X-Probe"])
}

// TestConfigureProviders_DoesNotMutateProcessEnviron is the direct P1.3
// regression test: running configureProviders with a real RUSH_-prefixed
// override present in the (real) process environment must not leave any
// trace in os.Environ() afterwards — neither the override leaking into
// the bare name, nor an unset var being left set to "".
func TestConfigureProviders_DoesNotMutateProcessEnviron(t *testing.T) {
	// UNSET_BEFORE has no process-level value at all before this test.
	// The historical PushPopCrushEnv bug would leave it set to "" after
	// "restoring" — os.Getenv can't tell "absent" from "set empty".
	_, hadUnsetBefore := os.LookupEnv("RUSH_ENV_OVERLAY_UNSET_BEFORE_PROBE")
	require.False(t, hadUnsetBefore, "test sanity: probe var must not already exist in this process")

	t.Setenv("RUSH_ENV_OVERLAY_UNSET_BEFORE_PROBE", "some-value")
	// t.Setenv already registers automatic cleanup (Unsetenv) for the
	// RUSH_-prefixed var; the bug this test targets is about the BARE
	// name (ENV_OVERLAY_UNSET_BEFORE_PROBE), which configureProviders
	// would have mutated via os.Setenv/os.Unsetenv under the old
	// PushPopCrushEnv implementation, and t.Setenv knows nothing about
	// that derived var.

	knownProviders := []catwalk.Provider{
		{
			ID:          "openai",
			APIKey:      "$OPENAI_API_KEY",
			APIEndpoint: "https://api.openai.com/v1",
			Models:      []catwalk.Model{{ID: "test-model"}},
		},
	}
	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	baseEnv := env.New() // osEnv — reads the REAL process environment.
	resolver := NewShellVariableResolver(baseEnv)

	before := os.Environ()
	err := cfg.configureProviders(context.Background(), testStore(cfg), baseEnv, resolver, knownProviders)
	require.NoError(t, err)
	after := os.Environ()

	require.ElementsMatch(t, before, after, "configureProviders must never mutate the real process environment")

	_, stillUnset := os.LookupEnv("ENV_OVERLAY_UNSET_BEFORE_PROBE")
	require.False(t, stillUnset, "a bare var that was never set must not become set (even to empty) as a side effect of the RUSH_ overlay")
}

// TestConfigureProviders_ConcurrentDifferentOverlays_NoInterference runs
// configureProviders concurrently from many goroutines, each with its own
// RUSH_-prefixed override for the SAME provider's API key template, and
// asserts every goroutine's resulting provider config reflects its OWN
// override — never another goroutine's — with no os.Environ() mutation
// visible to any of them. This is the P1.3 concurrency contract:
// PushPopCrushEnv's process-global os.Setenv/os.Unsetenv could let two
// concurrent ConfigStore reloads observe or clobber each other's
// override; the explicit-overlay replacement has no shared mutable state
// between calls, so this must never happen regardless of interleaving.
func TestConfigureProviders_ConcurrentDifferentOverlays_NoInterference(t *testing.T) {
	const goroutines = 16

	knownProvidersTemplate := func() []catwalk.Provider {
		return []catwalk.Provider{
			{
				ID:          "openai",
				APIKey:      "test-key",
				APIEndpoint: "https://api.openai.com/v1",
				Models:      []catwalk.Model{{ID: "test-model"}},
				DefaultHeaders: map[string]string{
					"X-Probe": "$OPENAI_API_KEY",
				},
			},
		}
	}

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			want := fmt.Sprintf("override-%d", i)
			cfg := &Config{}
			cfg.setDefaults(t.TempDir(), "")
			baseEnv := env.NewFromMap(map[string]string{
				"OPENAI_API_KEY":      "real-key",
				"RUSH_OPENAI_API_KEY": want,
			})
			resolver := NewShellVariableResolver(baseEnv)

			err := cfg.configureProviders(context.Background(), testStore(cfg), baseEnv, resolver, knownProvidersTemplate())
			require.NoError(t, err)

			pc, ok := cfg.Providers.Get("openai")
			require.True(t, ok)
			require.Equal(t, want, pc.ExtraHeaders["X-Probe"], "goroutine must see its own RUSH_ override, never another goroutine's")
		}(i)
	}
	wg.Wait()
}
