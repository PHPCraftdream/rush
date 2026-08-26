// Model-persistence tests: UpdateAgentAllowedTools copy-on-write generation
// publish, and the Load self-deadlock regression around persisting a
// corrected (fallback) model selection.

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/stretchr/testify/assert"
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

	// The configured smart/fast selection points at a provider that isn't
	// configured at all ("ghost-provider"), so configureSelectedModels must
	// fall back to and persist gpt-4 (the only configured provider's first
	// model) — exactly the path that triggers the reentrant autoReload call.
	//
	// It deliberately uses an unknown PROVIDER rather than an unknown model
	// under a known provider: since 2026-08-26 the latter is a supported
	// "unverified passthrough" (see unverifiedPassthroughModel in
	// load_providers.go — a model can be live before it reaches any cached
	// catalog) and no longer triggers the fallback+persist path this test
	// needs. An unconfigured provider is the remaining case that does.
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
			"smart": {"provider": "ghost-provider", "model": "not-a-real-model"},
			"fast": {"provider": "ghost-provider", "model": "not-a-real-model"}
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
		"a selection on an unconfigured provider must fall back to and persist the provider's first model")

	// The corrected selection must actually have reached disk (proving the
	// persist path really ran, not just the in-memory copy-on-write step).
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "gpt-4")
}

// TestLoad_UnverifiedModelOnKnownProviderSurvives pins the load-side half of
// the "accept an unverified provider/model" behavior (2026-08-26). A model
// that isn't in its provider's cached catalog — but whose provider IS
// configured — must survive config load untouched, and must NOT be
// overwritten on disk.
//
// Before unverifiedPassthroughModel existed, configureSelectedModels replaced
// such a selection with the provider's default AND persisted that replacement
// to the global scope, so merely running a read-only command like
// `rush models state` silently rewrote the user's chosen model. That made the
// `rush models use zai/<brand-new-model>` path (internal/app/provider.go)
// pointless: it accepted the value, then the next load discarded it.
func TestLoad_UnverifiedModelOnKnownProviderSurvives(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")

	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)

	// "brand-new-model" is absent from openai's model list here, exactly like
	// a model that shipped at the provider before any local catalog refresh.
	// disable_default_providers keeps the provider set offline/deterministic
	// (see the sibling test above for why that matters).
	initialConfig := `{
		"options": {"disable_default_providers": true},
		"models": {
			"smart": {"provider": "openai", "model": "brand-new-model", "reasoning_effort": "high"}
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

	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	smart := store.Config().Models[SelectedModelTypeSmart]
	require.Equal(t, "openai", smart.Provider)
	require.Equal(t, "brand-new-model", smart.Model,
		"a model missing from the catalog must survive when its provider is configured")
	require.Equal(t, "high", smart.ReasoningEffort,
		"the user's explicit effort must survive alongside the model")
	// Unknown metadata stays zero — "don't assume", the same convention
	// downstream readers already gate on (e.g. `if contextWindow > 0`).
	require.Zero(t, smart.MaxTokens)

	// Nothing may have been rewritten on disk.
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "brand-new-model")
	require.NotContains(t, string(data), `"model": "gpt-4"`,
		"the load path must not persist a substituted model over the user's choice")
}

// TestLoad_PartialSelectionSelfHealsInsteadOfMismatchedPassthrough is the
// regression test for an edge case a code review caught in
// unverifiedPassthroughModel: a PARTIAL selection (only provider or only
// model set, e.g. a hand-edited `"smart": {"provider": "zai"}` with no
// model) must still self-heal to a working default, not silently pair the
// explicitly-set provider with whatever model defaultModelSelection picked
// for some OTHER provider — a combination that would fail at actual request
// time with no diagnostic, and previously (before unverifiedPassthroughModel
// existed) always got the safe self-heal treatment.
func TestLoad_PartialSelectionSelfHealsInsteadOfMismatchedPassthrough(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")

	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)

	// "smart" names a real, configured provider (zai) but no model at all.
	// zai's own catalog only has "glm-5.3", so if the partial selection were
	// (wrongly) paired with openai's default model id, GetModel(zai, <that
	// id>) would miss and unverifiedPassthroughModel would (if not gated)
	// synthesize a passthrough for a model zai never advertised.
	initialConfig := `{
		"options": {"disable_default_providers": true},
		"models": {
			"smart": {"provider": "zai"}
		},
		"providers": {
			"openai": {
				"api_key": "test-key",
				"base_url": "https://example.invalid/v1",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			},
			"zai": {
				"api_key": "test-key",
				"base_url": "https://api.z.ai/v1",
				"models": [{"id": "glm-5.3", "name": "GLM 5.3"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// defaultModelSelection sorts enabled providers alphabetically when no
	// catwalk-fetched knownProviders list is available (disable_default_providers
	// is set) — "openai" sorts before "zai", so the fully self-healed default
	// is openai/gpt-4.
	smart := store.Config().Models[SelectedModelTypeSmart]
	require.Equal(t, "openai", smart.Provider,
		"a partial selection (provider set, model empty) must self-heal to a fully consistent default pair, not keep the explicitly-set provider paired with an unrelated model")
	require.Equal(t, "gpt-4", smart.Model)
	require.NotNil(t, store.Config().GetModel(smart.Provider, smart.Model),
		"the resulting selection must be a real, catalog-resolvable (provider, model) pair")
}

// TestLoad_UnverifiedModelOnKnownProviderSurvives_FastSlot mirrors
// TestLoad_UnverifiedModelOnKnownProviderSurvives for the FAST slot — a code
// review flagged that the fast-side call site of unverifiedPassthroughModel
// (load_providers.go, the `fastModelConfigured` branch) had no dedicated
// test: the smart and fast branches are near-duplicated by hand, not shared
// code, so a bug in only one of them (e.g. deleting just the fast-side call)
// would ship green under smart-only coverage.
func TestLoad_UnverifiedModelOnKnownProviderSurvives_FastSlot(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")

	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)

	initialConfig := `{
		"options": {"disable_default_providers": true},
		"models": {
			"fast": {"provider": "openai", "model": "brand-new-fast-model"}
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

	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	fast := store.Config().Models[SelectedModelTypeFast]
	require.Equal(t, "openai", fast.Provider)
	require.Equal(t, "brand-new-fast-model", fast.Model,
		"a model missing from the catalog must survive when its provider is configured, for the fast slot too")

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "brand-new-fast-model")
}

// TestUnverifiedPassthroughModel_DirectUnitTests exercises
// unverifiedPassthroughModel's own branches directly — the disabled-provider
// and missing-field guards are otherwise only reachable indirectly through
// configureSelectedModels' fallback path, which a code review flagged as
// leaving the inversion-prone guards themselves untested.
func TestUnverifiedPassthroughModel_DirectUnitTests(t *testing.T) {
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.Providers.Set("zai", ProviderConfig{ID: "zai", APIKey: "test-key"})
	cfg.Providers.Set("disabled-provider", ProviderConfig{ID: "disabled-provider", APIKey: "test-key", Disable: true})

	t.Run("known enabled provider, unlisted model -> synthesized stand-in", func(t *testing.T) {
		m := unverifiedPassthroughModel(cfg, SelectedModel{Provider: "zai", Model: "glm-5.3-flash"})
		require.NotNil(t, m)
		assert.Equal(t, "glm-5.3-flash", m.ID)
		assert.Equal(t, "glm-5.3-flash", m.Name)
		assert.Zero(t, m.ContextWindow, "unknown metadata must stay zero, never guessed")
	})

	t.Run("disabled provider -> nil, must not fabricate a stand-in", func(t *testing.T) {
		m := unverifiedPassthroughModel(cfg, SelectedModel{Provider: "disabled-provider", Model: "whatever"})
		assert.Nil(t, m)
	})

	t.Run("provider absent from config entirely -> nil", func(t *testing.T) {
		m := unverifiedPassthroughModel(cfg, SelectedModel{Provider: "ghost-provider", Model: "whatever"})
		assert.Nil(t, m)
	})

	t.Run("empty model -> nil", func(t *testing.T) {
		m := unverifiedPassthroughModel(cfg, SelectedModel{Provider: "zai", Model: ""})
		assert.Nil(t, m)
	})

	t.Run("empty provider -> nil", func(t *testing.T) {
		m := unverifiedPassthroughModel(cfg, SelectedModel{Provider: "", Model: "whatever"})
		assert.Nil(t, m)
	})
}
