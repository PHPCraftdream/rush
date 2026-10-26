// Round-2 slot-sibling regression tests (defects verified 2026-10):
//   - an unselected slot sibling that EXISTS on disk (rush.ktav selected,
//     ignored rush.json present) must contribute a real on-disk fingerprint
//     to the staleness snapshot; publishing Exists:false for an existing
//     file made ConfigStaleness report Dirty forever and drove endless
//     peak-hours reloads;
//   - slot-sibling expansion must cover the HIDDEN .rush.* spellings using
//     the same base-name list discovery walks;
//   - matchesLogicalSlot must normalize BOTH operands through legacySlotOf:
//     the anchor itself can already be a .ktav path (globalDataPath is
//     selected through selectConfigSlot).
package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
)

// seedGlobalFixture gives Load a valid offline provider via the global data
// slot so tests that keep the workspace dir free of visible rush.json files
// (hidden-slot scenarios) still load without touching catwalk.
func seedGlobalFixture(t *testing.T, globalPath string) {
	t.Helper()
	writeSignedModelsFixture(t, globalPath,
		`{"smart":{"provider":"openai","model":"gpt-global"}}`)
}

// writeHiddenKtavFixture is writeSignedKtavFixture, named for the hidden
// slot spellings these tests exercise.
func writeHiddenKtavFixture(t *testing.T, path, modelsJSON string) {
	t.Helper()
	writeSignedKtavFixture(t, path, modelsJSON)
}

// TestConfigStalenessUnselectedJSONSiblingStaysClean: rush.ktav AND an
// ignored rush.json both exist before Load. The snapshot must record
// Exists:true for the unselected sibling, so ConfigStaleness stays clean on
// unchanged disk across several natural ticks. FAILS against round 1:
// reloadStalenessState published the zero fingerprint (Exists:false) for
// the unselected sibling and ConfigStaleness reported Dirty on every call.
func TestConfigStalenessUnselectedJSONSiblingStaysClean(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedKtavFixture(t, ktavSiblingOf(configPath),
		`{"smart":{"provider":"openai","model":"gpt-ktav"}}`)
	writeSignedModelsFixture(t, configPath,
		`{"smart":{"provider":"openai","model":"gpt-json"}}`)
	seedGlobalFixture(t, globalPath)

	s, err := Load(dir, "", false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })
	require.Equal(t, "gpt-ktav", s.Config().Models[SelectedModelTypeSmart].Model,
		"the Ktav content must win the slot")

	require.False(t, s.ConfigStaleness().Dirty,
		"unchanged disk must not be dirty: the unselected rush.json sibling exists and must carry its real fingerprint")

	// Drive a reload: ReloadFromDisk rebuilds the published snapshot via
	// reloadStalenessState, whose fingerprints map only holds SELECTED
	// documents. FAILS against round 1 right here: the rebuilt snapshot
	// published Exists:false for the existing unselected sibling, so the
	// Dirty check below returned true and every later tick re-attempted a
	// reload of unchanged disk.
	require.NoError(t, s.ReloadFromDisk(context.Background()))
	require.False(t, s.ConfigStaleness().Dirty,
		"the rebuilt snapshot must record Exists:true for the unselected sibling")

	var st configDiskSyncState
	for i := 0; i < 3; i++ {
		result, err := s.syncConfigFromDisk(context.Background(), &st)
		require.NoError(t, err)
		require.False(t, result.Reloaded,
			"tick %d: an unchanged disk must not reload", i)
		require.False(t, s.ConfigStaleness().Dirty,
			"tick %d: staleness must stay clean across reloads", i)
	}
}

// TestConfigStalenessIgnoredJSONEditDirtyOnceThenSettles: modifying ONLY the
// ignored rush.json makes ConfigStaleness dirty (digest differs); one
// natural tick reloads, the ACTIVE config stays the Ktav content, and the
// store settles: Dirty returns to false and further ticks do not reload.
// Fails against round 1 in its settling half: the rebuilt snapshot kept
// publishing Exists:false for the sibling, so Dirty never returned to false
// and every subsequent tick re-attempted the reload.
func TestConfigStalenessIgnoredJSONEditDirtyOnceThenSettles(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedKtavFixture(t, ktavSiblingOf(configPath),
		`{"smart":{"provider":"openai","model":"gpt-ktav"}}`)
	writeSignedModelsFixture(t, configPath,
		`{"smart":{"provider":"openai","model":"gpt-json"}}`)
	seedGlobalFixture(t, globalPath)

	s, err := Load(dir, "", false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })

	var st configDiskSyncState
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.False(t, result.Reloaded, "initial disk state must be settled")

	// Foreign write to the ignored legacy sibling only.
	writeSignedModelsFixture(t, configPath,
		`{"fast":{"provider":"openai","model":"gpt-json-2"}}`)
	require.True(t, s.ConfigStaleness().Dirty,
		"the ignored sibling's digest changed; staleness must notice")

	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded, "a natural tick must reload exactly once")
	require.Equal(t, "gpt-ktav", s.Config().Models[SelectedModelTypeSmart].Model,
		"the ignored JSON edit must never change the active config")
	require.Equal(t, uint64(1), st.reloadAttempts)

	require.False(t, s.ConfigStaleness().Dirty, "the store must settle after the reload")
	for i := 0; i < 3; i++ {
		result, err := s.syncConfigFromDisk(context.Background(), &st)
		require.NoError(t, err)
		require.False(t, result.Reloaded,
			"tick %d: no endless reload after settling", i)
	}
	require.Equal(t, uint64(1), st.reloadAttempts, "reload attempts must stay bounded")
}

// TestConfigStalenessKtavEditReloadsAndChangesActiveConfig: modifying the
// SELECTED rush.ktav reloads and the active config changes.
func TestConfigStalenessKtavEditReloadsAndChangesActiveConfig(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedKtavFixture(t, ktavSiblingOf(configPath),
		`{"smart":{"provider":"openai","model":"gpt-ktav"}}`)
	seedGlobalFixture(t, globalPath)

	s, err := Load(dir, "", false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })

	var st configDiskSyncState
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.False(t, result.Reloaded)

	writeSignedKtavFixture(t, ktavSiblingOf(configPath),
		`{"smart":{"provider":"openai","model":"gpt-ktav-2"}}`)
	require.True(t, s.ConfigStaleness().Dirty)

	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded)
	require.Equal(t, "gpt-ktav-2", s.Config().Models[SelectedModelTypeSmart].Model,
		"the selected Ktav edit must reach the active config")
	require.False(t, s.ConfigStaleness().Dirty)
}

// TestConfigStalenessHiddenKtavSiblingNaturalTickReload: the hidden project
// slot <dir>/.rush.json is absent at Load; creating <dir>/.rush.ktav later
// must reload on a NATURAL tick (no force) and its content must reach the
// store. FAILS against round 1: expandConfigSlotSiblings only knew the
// visible rush.* pair, so <dir>/.rush.ktav was never tracked and the
// natural tick stayed quiescent.
func TestConfigStalenessHiddenKtavSiblingNaturalTickReload(t *testing.T) {
	dir, _, globalPath := configSyncIsolate(t)
	seedGlobalFixture(t, globalPath)
	// No visible dir/rush.json: the hidden spellings are the only project
	// candidates, and discovery tracks them even while absent.

	s, err := Load(dir, "", false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })
	require.Contains(t, s.loadSnapshot().trackedConfigPaths,
		canonicalConfigPath(filepath.Join(dir, ".rush.json")),
		"the absent hidden slot must be tracked")

	require.False(t, s.ConfigStaleness().Dirty)

	hiddenKtav := filepath.Join(dir, ".rush.ktav")
	writeHiddenKtavFixture(t, hiddenKtav,
		`{"fast":{"provider":"openai","model":"gpt-hidden"}}`)

	var st configDiskSyncState
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded,
		"a natural tick must reload when the hidden Ktav spelling appears")
	require.Equal(t, "gpt-hidden", s.Config().Models[SelectedModelTypeFast].Model,
		"the hidden slot's content must reach the store")
	require.False(t, s.ConfigStaleness().Dirty)

	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.False(t, result.Reloaded, "drift must be exactly-once per event")
}

// TestConfigStalenessHiddenPairUnselectedEditDirtyOnce: hidden .rush.ktav
// and hidden .rush.json coexisting selects the Ktav spelling; modifying the
// unselected hidden JSON makes ConfigStaleness dirty exactly once, the
// active config stays the Ktav content, and the store settles.
func TestConfigStalenessHiddenPairUnselectedEditDirtyOnce(t *testing.T) {
	dir, _, globalPath := configSyncIsolate(t)
	seedGlobalFixture(t, globalPath)
	hiddenKtav := filepath.Join(dir, ".rush.ktav")
	hiddenJSON := filepath.Join(dir, ".rush.json")
	writeHiddenKtavFixture(t, hiddenKtav,
		`{"smart":{"provider":"openai","model":"gpt-hidden-ktav"}}`)
	writeSignedModelsFixture(t, hiddenJSON,
		`{"smart":{"provider":"openai","model":"gpt-hidden-json"}}`)

	s, err := Load(dir, "", false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })
	require.Equal(t, "gpt-hidden-ktav", s.Config().Models[SelectedModelTypeSmart].Model)

	require.False(t, s.ConfigStaleness().Dirty,
		"the unselected hidden JSON sibling exists and must carry its real fingerprint")

	writeSignedModelsFixture(t, hiddenJSON,
		`{"fast":{"provider":"openai","model":"gpt-hidden-json-2"}}`)
	require.True(t, s.ConfigStaleness().Dirty)

	var st configDiskSyncState
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded)
	require.Equal(t, "gpt-hidden-ktav", s.Config().Models[SelectedModelTypeSmart].Model,
		"the unselected hidden JSON edit must never change the active config")
	require.False(t, s.ConfigStaleness().Dirty)

	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.False(t, result.Reloaded, "the store must settle")
}

// TestMatchesLogicalSlotBothOperandsNormalized: matchesLogicalSlot must
// normalize BOTH operands through legacySlotOf. The ktav-anchor/legacy-
// canonical combination FAILS against round 1: it compared rush.json
// against ktavSiblingOf(rush.ktav) == rush.ktav.ktav and returned false.
func TestMatchesLogicalSlotBothOperandsNormalized(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "rush.json")
	ktav := legacy[:len(legacy)-len(".json")] + ktavConfigExt
	other := filepath.Join(dir, "other.json")

	require.True(t, matchesLogicalSlot(legacy, legacy), "legacy/legacy")
	require.True(t, matchesLogicalSlot(ktav, legacy), "ktav vs legacy anchor")
	require.True(t, matchesLogicalSlot(legacy, ktav),
		"legacy vs KTAV anchor (round 1 returned false here)")
	require.True(t, matchesLogicalSlot(ktav, ktav), "ktav/ktav")
	require.False(t, matchesLogicalSlot(other, legacy), "non-slot path")
	require.False(t, matchesLogicalSlot(legacy, ""), "empty anchor never matches")
	require.False(t, matchesLogicalSlot("", legacy), "empty canonical never matches")
}

// TestMCPGlobalKtavSlotWritableAfterInit: the global data slot exists as
// rush.ktav BEFORE Init, with an MCP server definition inside, and a valid
// custom provider seeded beside it (config-dir rush.json, migrated to
// rush.ktav by Init under the auto-migrate gate — expected). After Init the
// global slot must resolve writable and accept disable/enable overrides.
// No t.Parallel: this test mutates process environment variables.
func TestMCPGlobalKtavSlotWritableAfterInit(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "global-config")
	dataDir := filepath.Join(root, "global-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("RUSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	// Provider fixture in the config dir (state_regression_test.go pattern);
	// Init migrates it to rush.ktav under the gate — expected, not asserted.
	writeSignedModelsFixture(t, filepath.Join(configDir, "rush.json"),
		`{"smart":{"provider":"openai","model":"gpt-config"}}`)

	// The global data slot pre-seeded as rush.ktav with an MCP definition.
	doc := `{"options":{"disable_default_providers":true},` +
		`"models":{"smart":{"provider":"openai","model":"gpt-global"}},` +
		`"providers":{"openai":{"api_key":"test-key","base_url":"https://example.invalid/v1","models":[{"id":"gpt-4","name":"GPT-4"}]}},` +
		`"mcp":{"testsvc":{"type":"http","url":"http://example.com"}}}`
	signed, err := applyModelsIntegrity(doc)
	require.NoError(t, err)
	ktav, err := ktavcodec.ConvertJSONToKtav([]byte(signed))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.ktav"), ktav, 0o600))

	s, err := Init(root, root, false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })
	require.Equal(t, "gpt-global", s.Config().Models[SelectedModelTypeSmart].Model,
		"the pre-existing global rush.ktav must be the selected slot")

	scope, err := s.ResolveMCPWritableScope("testsvc")
	require.NoError(t, err)
	require.Equal(t, ScopeGlobal, scope)
	require.NoError(t, s.PersistMCPDisabledOverride(ScopeGlobal, "testsvc", true))
	require.NoError(t, s.PersistMCPDisabledOverride(ScopeGlobal, "testsvc", false))
}
