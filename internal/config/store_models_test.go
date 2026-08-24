// Model-persistence tests: UpdateAgentAllowedTools copy-on-write generation
// publish, and the Load self-deadlock regression around persisting a
// corrected (fallback) model selection.

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestConfigStore_UpdateAgentAllowedTools_PublishesNewGenerationWithoutMutatingOldSnapshot
// pins the copy-on-write contract of UpdateAgentAllowedTools: a *Config
// pointer captured via Config() BEFORE the call must NOT observe the
// change (it's a distinct, frozen generation), while a Config() call AFTER
// sees the update. This is the regression test for the bug UpdateAgentAllowedTools
// replaces — the old disableToolsInConfig wrote straight into
// cfg.Agents[id] on the currently-published *Config, which meant any
// reader holding that same pointer from before the call would see the
// mutation retroactively.
func TestConfigStore_UpdateAgentAllowedTools_PublishesNewGenerationWithoutMutatingOldSnapshot(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		config: &Config{
			Agents: map[string]Agent{
				AgentCoder: {
					ID:           AgentCoder,
					AllowedTools: []string{"view", "grep", "agent", "agentic_fetch", "bash", "edit"},
				},
			},
		},
	})

	// Capture the *Config pointer BEFORE the mutation, simulating a
	// concurrent reader that read Config() earlier and is still holding
	// that pointer (e.g. mid-turn).
	oldCfg := store.Config()
	oldTools := oldCfg.Agents[AgentCoder].AllowedTools

	store.UpdateAgentAllowedTools(AgentCoder, []string{"view", "grep", "bash", "edit"})

	// The old snapshot's Config must be completely unaffected: neither its
	// Agents map entry nor the AllowedTools slice it pointed to should
	// have changed underneath the earlier reader.
	require.Equal(t, []string{"view", "grep", "agent", "agentic_fetch", "bash", "edit"}, oldCfg.Agents[AgentCoder].AllowedTools,
		"a *Config captured before UpdateAgentAllowedTools must not observe the mutation")
	require.Equal(t, []string{"view", "grep", "agent", "agentic_fetch", "bash", "edit"}, oldTools,
		"the AllowedTools slice captured before the call must not be touched in place")

	// A fresh Config() call after the mutation sees the new generation.
	newCfg := store.Config()
	require.Equal(t, []string{"view", "grep", "bash", "edit"}, newCfg.Agents[AgentCoder].AllowedTools,
		"Config() after UpdateAgentAllowedTools must see the new generation")

	// The two *Config pointers must be distinct — proof that a new
	// generation was published rather than the old one mutated in place.
	require.NotSame(t, oldCfg, newCfg, "UpdateAgentAllowedTools must publish a new *Config, not mutate the old one")
}

// TestLoad_PersistingCorrectedModelDoesNotDeadlock is the regression test
// for a self-deadlock discovered via TestModelsBump_NonAtomModel_ReportsCleanly
// (internal/cmd) hanging past both a 600s full-suite timeout and an isolated
// 30s single-test timeout, with an identical stuck stack both times.
//
// Root cause: Load holds publishMu for its entire body. When a configured
// model can't be resolved (GetModel returns nil), configureSelectedModels
// persists a corrected default via updatePreferredModelLocked ->
// updatePreferredModelsLocked -> SetConfigFields, and SetConfigFields
// unconditionally calls autoReload after its disk write. autoReload's own
// redundant-reload dedup is reloadMu.TryLock() (not publishMu, since commit
// 682cf21a) — since Load didn't hold reloadMu, the re-entrant autoReload
// call would succeed that TryLock and hang forever trying to re-acquire
// publishMu on the same goroutine that Load already holds it on.
//
// Fixed by having Load also hold reloadMu for its whole body (same
// acquisition order as buildAndPublishReload: reloadMu -> publishMu), so
// autoReload's TryLock(reloadMu) correctly fails and skips instead of
// recursing — restoring the original pre-682cf21a safety property using the
// new lock.
func TestLoad_PersistingCorrectedModelDoesNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")

	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)

	// The configured smart/fast model ("not-a-real-model") does not exist
	// in the provider's model list, so configureSelectedModels must fall
	// back to and persist gpt-4 (the provider's first model) — exactly the
	// path that triggers the reentrant autoReload call.
	//
	// disable_default_providers is set so Providers(cfg) skips its
	// catwalk-catalog fetch entirely (Providers() memoizes its result in a
	// package-level sync.Once for the whole test binary, and would
	// otherwise hit the real network/cache for a live provider list — this
	// test needs a fully offline, deterministic provider set, since it
	// asserts an exact fallback model id).
	initialConfig := `{
		"options": {"disable_default_providers": true},
		"models": {
			"smart": {"provider": "openai", "model": "not-a-real-model"},
			"fast": {"provider": "openai", "model": "not-a-real-model"}
		},
		"providers": {
			"openai": {
				"api_key": "test-key",
				"base_url": "https://example.invalid/v1",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	done := make(chan struct{})
	var store *ConfigStore
	var loadErr error
	go func() {
		defer close(done)
		store, loadErr = Load(dir, dir, false)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Load hung past 10s — regression: SetConfigFields' autoReload call " +
			"deadlocked against Load's held publishMu (see Load's doc comment in load.go)")
	}

	require.NoError(t, loadErr)
	require.Equal(t, "openai", store.Config().Models[SelectedModelTypeSmart].Provider)
	require.Equal(t, "gpt-4", store.Config().Models[SelectedModelTypeSmart].Model,
		"unresolvable configured model must fall back to and persist the provider's first model")

	// The corrected selection must actually have reached disk (proving the
	// persist path really ran, not just the in-memory copy-on-write step).
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "gpt-4")
}
