// Gate-on coverage: a symlinked rush.json must defer ktav migration
// without publishing any transaction artifacts.
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestKtavLoadDefersSymlinkedLegacyConfig verifies that a rush.json
// symlink never migrates: Load succeeds, no Ktav file or migration
// record appears, and the link plus its physical target stay intact.
func TestKtavLoadDefersSymlinkedLegacyConfig(t *testing.T) {
	enableKtavAutoMigrateForTest(t)
	_, _ = isolateAllGlobalConfigPaths(t)
	dir := t.TempDir()
	physical := filepath.Join(dir, "physical", "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(physical), 0o755))
	original := []byte(`{"providers":{"custom":{"base_url":"https://example.invalid/v1","api_key":"test-key","models":[{"id":"model","name":"Model"}]}},"options":{"disable_default_providers":true}}`)
	require.NoError(t, os.WriteFile(physical, original, 0o600))
	logical := filepath.Join(dir, "rush.json")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("logical rush.json symlink unavailable: %v", err)
	}

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.NotNil(t, store)

	require.NoFileExists(t, ktavSiblingOf(logical))
	require.NoFileExists(t, migrationRecordPathFor(logical))
	require.NoFileExists(t, backupPathFor(logical))

	linkInfo, err := os.Lstat(logical)
	require.NoError(t, err)
	require.NotEqual(t, os.FileMode(0), linkInfo.Mode()&os.ModeSymlink)
	target, err := os.Readlink(logical)
	require.NoError(t, err)
	require.Equal(t, physical, target)
	persisted, err := os.ReadFile(physical)
	require.NoError(t, err)
	require.Equal(t, original, persisted)
}
