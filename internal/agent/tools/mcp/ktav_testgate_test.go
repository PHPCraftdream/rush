package mcp

// Ktav test gate: the mcp package's tests are overwhelmingly legacy
// rush.json fixture tests — they write rush.json, then assert rush.json on
// disk. With Ktav auto-migration active (the shipped default since the
// activation flip), config.Init renames rush.json to rush.json.bak and
// mutations land in rush.ktav, so those assertions fail. This init()
// therefore pins the WHOLE package to the pre-activation world
// (ktavAutoMigrate = false) so every legacy fixture asserts exactly what it
// asserted before. The restore func is intentionally ignored: the gate stays
// off for the life of the test binary.
//
// The shipped default (gate ON) is covered by the explicit opt-in test
// TestKtavAutoMigrateActiveShippedDefault in this file, which uses
// enableKtavForTest. Any future test that asserts rush.ktav selection,
// mutation or migration MUST call enableKtavForTest(t) — and must NOT use
// t.Parallel, because the gate is package-global state.
//
// There is already a TestMain in process_windows_test.go: do NOT add
// another one; init() is the mechanism.
import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func init() {
	config.SetKtavAutoMigrateForTest(false)
}

// enableKtavForTest opts a single test into the shipped Ktav-on default.
// Never combine with t.Parallel: the gate is package-global state and the
// restore runs via t.Cleanup.
func enableKtavForTest(t *testing.T) func() {
	t.Helper()
	return config.SetKtavAutoMigrateForTest(true)
}

func TestKtavAutoMigrateActiveShippedDefault(t *testing.T) {
	restore := enableKtavForTest(t)
	defer restore()

	store := isolatedMCPStore(t)
	configDir := os.Getenv("RUSH_GLOBAL_CONFIG")

	// Init migrated the legacy global rush.json to Ktav and kept a backup.
	_, err := os.Stat(filepath.Join(configDir, "rush.ktav"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(configDir, "rush.json"))
	require.True(t, os.IsNotExist(err), "legacy rush.json must be renamed away once migrated")
	_, err = os.Stat(filepath.Join(configDir, "rush.json.bak"))
	require.NoError(t, err)

	// Mutations land in the Ktav world and never resurrect a legacy rush.json.
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "mcp.ktav-default-probe", config.MCPConfig{
		Type:    config.MCPHttp,
		URL:     "http://ktav-default.example",
		Timeout: 60,
	}))
	var legacyResurrected bool
	err = filepath.WalkDir(configDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(data, []byte("ktav-default-probe")) && filepath.Base(path) == "rush.json" {
			legacyResurrected = true
		}
		return nil
	})
	require.NoError(t, err)
	require.False(t, legacyResurrected, "Ktav-on mutation must not write a legacy rush.json")
	_, exists := store.MCPConfig("ktav-default-probe")
	require.True(t, exists)
}
