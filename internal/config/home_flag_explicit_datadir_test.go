package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

// TestLoad_HomeFlag_ExplicitDataDirAtMainRush: shared-from-linked is defined by the DIRECTORY (data dir == <main>/.rush inside a linked worktree), not by which resolution step picked it. An explicit --data-dir or options.data_directory pointing at <main>/.rush makes the process not home and bars a dev build from migrating (WS-1: legacy unbound session rows belong to home processes only).
//
// Revert-check: restoring the source-only rule (workspaceHomeFlag.Store(dataDirSource != DataDirSourceShared) in load.go and s.DataDirSource() != DataDirSourceShared in ConfigStore.WorkspaceHome/MayMigrateData) makes the two explicit-at-main subtests fail while the other three still pass.
func TestLoad_HomeFlag_ExplicitDataDirAtMainRush(t *testing.T) {
	if _, err := platform.Command(context.Background(), "git", "version").Output(); err != nil {
		t.Skip("git not available")
	}
	allowRedirection(t)
	isolateAllGlobalConfigPaths(t)
	t.Cleanup(func() { setDataDirHomeForTest(true) })

	t.Run("flag data dir at the main .rush is not home", func(t *testing.T) {
		mainRoot, wtRoot := makeMainRepoWithWorktree(t)
		mainData := filepath.Join(mainRoot, ".rush")
		t.Cleanup(setOSExecutablePathForTest(filepath.Join(t.TempDir(), "tmp-exe", "rush.exe")))

		store, err := Load(wtRoot, mainData, false)
		require.NoError(t, err)
		require.Equal(t, DataDirSourceFlag, store.DataDirSource())
		require.Equal(t, mainData, store.Config().Options.DataDirectory)
		require.False(t, store.WorkspaceHome())
		require.False(t, WorkspaceHome(), "process flag must follow the directory, not the source")
		require.False(t, store.MayMigrateData(), "a dev build on the shared directory must not migrate")
	})

	t.Run("config data_directory at the main .rush is not home", func(t *testing.T) {
		mainRoot, wtRoot := makeMainRepoWithWorktree(t)
		mainData := filepath.Join(mainRoot, ".rush")
		t.Cleanup(setOSExecutablePathForTest(filepath.Join(t.TempDir(), "tmp-exe", "rush.exe")))

		body, err := json.Marshal(map[string]any{
			"options": map[string]any{"data_directory": mainData},
		})
		require.NoError(t, err)
		cfgPath := filepath.Join(wtRoot, "rush.json")
		require.NoError(t, os.WriteFile(cfgPath, body, 0o644))
		t.Cleanup(func() { os.Remove(cfgPath) })

		store, err := Load(wtRoot, "", false)
		require.NoError(t, err)
		require.Equal(t, DataDirSourceConfig, store.DataDirSource())
		require.Equal(t, mainData, store.Config().Options.DataDirectory)
		require.False(t, store.WorkspaceHome())
		require.False(t, WorkspaceHome(), "process flag must follow the directory, not the source")
		require.False(t, store.MayMigrateData(), "a dev build on the shared directory must not migrate")
	})

	t.Run("flag data dir elsewhere in a linked worktree stays home", func(t *testing.T) {
		_, wtRoot := makeMainRepoWithWorktree(t)
		other := filepath.Join(t.TempDir(), "other-data")

		store, err := Load(wtRoot, other, false)
		require.NoError(t, err)
		require.Equal(t, DataDirSourceFlag, store.DataDirSource())
		require.True(t, store.WorkspaceHome())
		require.True(t, WorkspaceHome(), "process flag must follow the directory, not the source")
		require.True(t, store.MayMigrateData())
	})

	t.Run("ordinary main checkout keeps explicit data dirs home", func(t *testing.T) {
		mainRoot, _ := makeMainRepoWithWorktree(t)
		mainData := filepath.Join(mainRoot, ".rush")

		store, err := Load(mainRoot, mainData, false)
		require.NoError(t, err)
		require.Equal(t, DataDirSourceFlag, store.DataDirSource())
		require.Equal(t, mainData, store.Config().Options.DataDirectory)
		require.True(t, store.WorkspaceHome(), "operator guarantee: explicit --data-dir in an ordinary checkout stays home")

		temp := filepath.Join(t.TempDir(), "temp-data")
		store2, err := Load(mainRoot, temp, false)
		require.NoError(t, err)
		require.True(t, store2.WorkspaceHome())
	})

	t.Run("default resolutions keep the old answers", func(t *testing.T) {
		t.Run("redirected shared default is not home", func(t *testing.T) {
			deployedExeSeam(t)
			mainRoot, wtRoot := makeMainRepoWithWorktree(t)

			store, err := Load(wtRoot, "", false)
			require.NoError(t, err)
			require.Equal(t, DataDirSourceShared, store.DataDirSource())
			require.Equal(t, filepath.Join(mainRoot, ".rush"), store.Config().Options.DataDirectory)
			require.False(t, store.WorkspaceHome())
		})

		t.Run("plain checkout default is home", func(t *testing.T) {
			plain := t.TempDir()

			store, err := Load(plain, "", false)
			require.NoError(t, err)
			require.Equal(t, DataDirSourceDefault, store.DataDirSource())
			require.True(t, store.WorkspaceHome())
		})
	})
}
