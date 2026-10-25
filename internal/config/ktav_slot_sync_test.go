// Ktav slot-sibling regression tests (defects verified 2026-10):
//   - the config disk-sync poller must see foreign writes to either
//     spelling (rush.json / rush.ktav) of a logical config slot on NATURAL
//     ticks, not only on the every-12th forced drift check;
//   - a mid-session migration that renames the global rush.json to
//     rush.ktav must keep the global MCP slot writable even though
//     ConfigStore.globalDataPath still anchors the legacy spelling.
package config

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
)

// writeSignedKtavFixture writes a signed rush config fixture in Ktav form.
func writeSignedKtavFixture(t *testing.T, path, modelsJSON string) {
	t.Helper()
	doc := `{"options":{"disable_default_providers":true},"models":` + modelsJSON +
		`,"providers":{"openai":{"api_key":"test-key","base_url":"https://example.invalid/v1","models":[{"id":"gpt-4","name":"GPT-4"}]}}}`
	signed, err := applyModelsIntegrity(doc)
	require.NoError(t, err)
	ktav, err := ktavcodec.ConvertJSONToKtav([]byte(signed))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, ktav, 0o600))
}

// TestConfigDiskSyncNaturalTickSeesForeignKtavSiblingWrite: a store loaded
// with an absent global slot tracks the legacy rush.json name; a foreign
// process creating rush.ktav must trigger a reload on the next NATURAL
// tick, and the new spelling must join the tracked set. Without
// expandConfigSlotSiblings the stat and digest gates only examine the old
// tracked paths, so the natural tick stays quiescent.
func TestConfigDiskSyncNaturalTickSeesForeignKtavSiblingWrite(t *testing.T) {
	pinKtavAutoMigrateOffForLegacyTest(t)
	dir, _, globalPath := configSyncIsolate(t)
	writeSignedModelsFixture(t, filepath.Join(dir, "rush.json"),
		`{"smart":{"provider":"openai","model":"gpt-project"}}`)
	s, err := Load(dir, "", false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })
	require.Equal(t, "gpt-project", s.Config().Models[SelectedModelTypeSmart].Model)
	require.NoFileExists(t, globalPath)

	var st configDiskSyncState
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.False(t, result.Reloaded, "an unchanged disk must not reload")

	// Foreign write: the Ktav spelling of the still-absent global slot
	// appears. Every previously tracked file is byte-identical (the global
	// rush.json was absent and stays absent).
	writeSignedKtavFixture(t, ktavSiblingOf(globalPath),
		`{"fast":{"provider":"openai","model":"gpt-global"}}`)

	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded,
		"a natural tick must reload when the Ktav sibling of a tracked slot appears")
	require.Equal(t, "gpt-global", s.Config().Models[SelectedModelTypeFast].Model,
		"the foreign Ktav write must reach the store")
	require.True(t, slices.Contains(s.loadSnapshot().trackedConfigPaths,
		canonicalConfigPath(ktavSiblingOf(globalPath))),
		"the Ktav sibling must join the tracked set")

	// Settled again: a further natural tick stays quiescent.
	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.False(t, result.Reloaded, "drift must be exactly-once per event")
}

// TestConfigDiskSyncForeignLegacyWriteKeepsKtavContentAndPollerLive: the
// global slot is loaded from rush.ktav; a foreign rush.json appearing
// beside it must not change the active config content (Ktav wins) and must
// not wedge the poller: the natural tick reloads once and settles.
func TestConfigDiskSyncForeignLegacyWriteKeepsKtavContentAndPollerLive(t *testing.T) {
	pinKtavAutoMigrateOffForLegacyTest(t)
	dir, _, globalPath := configSyncIsolate(t)
	writeSignedKtavFixture(t, ktavSiblingOf(globalPath),
		`{"smart":{"provider":"openai","model":"gpt-ktav"}}`)
	s, err := Load(dir, "", false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })
	require.Equal(t, "gpt-ktav", s.Config().Models[SelectedModelTypeSmart].Model)

	var st configDiskSyncState
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.False(t, result.Reloaded, "an unchanged disk must not reload")

	// Foreign write: the legacy JSON spelling of the Ktav-anchored slot.
	writeSignedModelsFixture(t, globalPath,
		`{"smart":{"provider":"openai","model":"gpt-legacy-json"}}`)

	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded,
		"a natural tick must consume the foreign legacy write instead of ignoring it")
	require.Equal(t, "gpt-ktav", s.Config().Models[SelectedModelTypeSmart].Model,
		"the active Ktav content must survive the foreign legacy write")

	// Settled again: a further natural tick stays quiescent.
	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.False(t, result.Reloaded, "the poller must not loop on the same foreign write")
}

// TestMCPGlobalScopeWritableAfterMidSessionKtavMigration: the global config
// file is absent at Load (globalDataPath anchors dataDir/rush.json), then a
// rush.json with an MCP server definition appears and the reload's startup
// migration renames it to rush.ktav. evaluateMCPFiles sees the server under
// the Ktav spelling, so the origin must still resolve through
// matchesLogicalSlot and stay writable; Disable/Enable must succeed.
func TestMCPGlobalScopeWritableAfterMidSessionKtavMigration(t *testing.T) {
	store, _ := isolatedMCPConfigStore(t)
	globalPath := GlobalConfigData()
	require.NoFileExists(t, globalPath)

	writeMCPDefinition(t, globalPath, "global-migrated")
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	require.FileExists(t, ktavSiblingOf(globalPath),
		"the reload must migrate the global rush.json to rush.ktav")
	require.NoFileExists(t, globalPath)

	scope, err := store.ResolveMCPWritableScope("global-migrated")
	require.NoError(t, err)
	require.Equal(t, ScopeGlobal, scope,
		"the migrated global slot must stay writable through its Ktav spelling")
	require.NoError(t, store.PersistMCPDisabledOverride(ScopeGlobal, "global-migrated", true))
	require.NoError(t, store.PersistMCPDisabledOverride(ScopeGlobal, "global-migrated", false))
}
