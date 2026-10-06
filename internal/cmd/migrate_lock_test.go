package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// `rush migrate` must honour a standing settings lock: refuse (non-zero) without
// changing anything, stay usable as a dry run, and work for an authorized process.
// Revert-check: drop the SettingsWriteAllowed calls in migrate.go => red.
func TestMigrateRespectsSettingsLock(t *testing.T) {
	_, dataDir := isolateGlobalPaths(t)
	lock := []byte(`{"sync_rev":"` + config.HashPassword("pw") + `"}`)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), lock, 0o600))
	config.SetProcessPassword("")
	t.Cleanup(func() {
		config.SetProcessPassword("")
		_ = migrateCmd.Flags().Set("dry-run", "false")
	})

	seed := func(t *testing.T) (root, legacy string) {
		t.Helper()
		root = t.TempDir()
		legacy = filepath.Join(root, "crush.json")
		require.NoError(t, os.WriteFile(legacy, []byte(`{"root":"config"}`), 0o644))
		return root, legacy
	}
	run := func(root string, dry bool) error {
		require.NoError(t, migrateCmd.Flags().Set("dry-run", map[bool]string{true: "true", false: "false"}[dry]))
		var out bytes.Buffer
		migrateCmd.SetOut(&out)
		migrateCmd.SetErr(&out)
		migrateCmd.SetIn(bytes.NewReader(nil))
		return migrateCmd.RunE(migrateCmd, []string{root})
	}

	t.Run("refused", func(t *testing.T) {
		root, legacy := seed(t)
		require.ErrorIs(t, run(root, false), config.ErrSettingsLocked)
		_, err := os.Stat(legacy)
		require.NoError(t, err, "the legacy file must stay")
		_, err = os.Stat(filepath.Join(root, ".rush"))
		require.True(t, os.IsNotExist(err), "no rush settings may be created")
	})
	t.Run("dry run still works", func(t *testing.T) {
		root, legacy := seed(t)
		require.NoError(t, run(root, true))
		_, err := os.Stat(legacy)
		require.NoError(t, err)
	})
	t.Run("authorized process migrates", func(t *testing.T) {
		root, legacy := seed(t)
		config.SetProcessPassword("pw")
		require.NoError(t, run(root, false))
		_, err := os.Stat(legacy)
		require.True(t, os.IsNotExist(err), "the legacy file must be renamed")
	})
}
