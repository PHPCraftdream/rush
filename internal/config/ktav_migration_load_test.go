package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/stretchr/testify/require"
)

// ktavSetupLoadMigrationTest isolates process-global config paths and disables
// default provider discovery so Load can run without network access.
func ktavSetupLoadMigrationTest(t *testing.T, migrateJSON bool) {
	t.Helper()
	globalConfig, globalData := isolateAllGlobalConfigPaths(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("RUSH_GLOBAL_CONFIG", globalConfig)
	t.Setenv("RUSH_GLOBAL_DATA", globalData)
	t.Setenv("RUSH_DISABLE_DEFAULT_PROVIDERS", "true")
	for _, path := range []string{globalConfig, globalData} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	savedGate := ktavAutoMigrate
	ktavAutoMigrate = migrateJSON
	t.Cleanup(func() { ktavAutoMigrate = savedGate })
}

func ktavLoadMigrationTestConfig(t *testing.T, dir string, migrateJSON bool) *ConfigStore {
	t.Helper()
	ktavSetupLoadMigrationTest(t, migrateJSON)
	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.NotNil(t, store)
	return store
}

// TestKtavLoadDeferredMigration verifies a lock failure defers project migration
// without making Load fail or altering the active JSON.
func TestKtavLoadDeferredMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "rush.json")
	original := []byte(`{"options":{"debug":true}}`)
	require.NoError(t, os.WriteFile(legacy, original, 0o600))
	ktavSetupLoadMigrationTest(t, true)

	configTestHooks.Lock()
	previousAcquire := configTestHooks.acquireConfigLock
	projectLockPath := normalizeReloadPath(legacy) + ".lock"
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*filelock.FileLock, error) {
		if normalizeReloadPath(path) == projectLockPath {
			return nil, os.ErrPermission
		}
		return filelock.AcquireFileLockContext(ctx, path)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.acquireConfigLock = previousAcquire
		configTestHooks.Unlock()
	})

	logs := captureSlogOutput(t)
	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.True(t, store.Config().Options.Debug)
	persisted, err := os.ReadFile(legacy)
	require.NoError(t, err)
	require.Equal(t, original, persisted)
	require.NoFileExists(t, filepath.Join(dir, "rush.ktav"))
	require.NoFileExists(t, filepath.Join(dir, "rush.json.bak"))
	require.Contains(t, logs.String(), "deferred")
}
