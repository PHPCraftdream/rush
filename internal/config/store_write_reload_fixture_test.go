package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Load читает global/MCP slots даже при локальном globalDataPath override.
func newWriteReloadFixture(t *testing.T, initial string) (*ConfigStore, string) {
	t.Helper()
	ktavLightIsolateEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))
	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = path
	store.CaptureStalenessSnapshot([]string{path})
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		result := store.ConfigStaleness()
		snapshot := store.loadSnapshot()
		t.Logf("auto-reload staleness: generation=%d dirty=%v changed=%v missing=%v errors=%v", snapshot.generation, result.Dirty, result.Changed, result.Missing, result.Errors)
		for i, path := range snapshot.trackedConfigPaths {
			if i == 16 {
				t.Log("remaining tracked paths omitted")
				break
			}
			actual, err := trackedSlotFingerprint(path, nil)
			expected := snapshot.snapshots[path]
			t.Logf("path=%s expected_exists=%v actual_exists=%v size=%d/%d mtime=%d/%d fingerprint_equal=%v error=%v", path, expected.Exists, actual.exists, expected.Size, actual.size, expected.ModTime, actual.modTime, expected.fingerprint == actual, err)
		}
	})
	return store, path
}

func TestWriteReloadFixtureDoesNotTrackForeignGlobalSlot(t *testing.T) {
	foreignDir := t.TempDir()
	foreign := filepath.Join(foreignDir, "rush.json")
	t.Setenv("RUSH_GLOBAL_CONFIG", foreignDir)
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	require.NoError(t, os.WriteFile(foreign, []byte(`{"options":{"debug":false}}`), 0o600))
	store, _ := newWriteReloadFixture(t, `{"options":{"debug":false}}`)
	require.NoError(t, store.SetConfigField(ScopeGlobal, "options.debug", true))
	require.True(t, store.Config().Options.Debug)
	// Имитируем соседний test/process, изменивший общий slot после reload.
	require.NoError(t, os.WriteFile(foreign, []byte(`{"options":{"debug":true}}`), 0o600))
	require.False(t, store.ConfigStaleness().Dirty, "unowned global slots must not enter this fixture's clean-state assertion")
}

func TestWriteReloadFixtureStillDetectsOwnExternalEdit(t *testing.T) {
	store, path := newWriteReloadFixture(t, `{"options":{"debug":false}}`)
	require.NoError(t, store.SetConfigField(ScopeGlobal, "options.debug", true))
	require.False(t, store.ConfigStaleness().Dirty)
	require.NoError(t, os.WriteFile(ActiveConfigFileName(path), []byte("options: { debug: false }\n"), 0o600))
	require.True(t, store.ConfigStaleness().Dirty, "isolation must not disable external-change detection")
}
