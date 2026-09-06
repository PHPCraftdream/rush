//go:build !windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnixCommitExpectedAbsentNoReplaceClosesCheckCommitRace(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	_, expected, err := readStableConfigFile(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	configTestHooks.Lock()
	previous := configTestHooks.beforeCommitRename
	configTestHooks.beforeCommitRename = func() {
		_ = os.WriteFile(path, []byte(`{"winner":"other"}`), 0o600)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeCommitRename = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, []byte(`{"winner":"ours"}`), 0o600, expected, 0, false)
	require.Error(t, err)
	require.True(t, errors.Is(err, errConfigCommitVerification))
	require.Equal(t, []byte(`{"winner":"other"}`), mustReadFile(t, path))
}

func TestGenericConfigMutationRejectsHardLink(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	alias := filepath.Join(root, "alias.json")
	contents := []byte(`{"options":{"debug":false}}`)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	require.NoError(t, os.Link(path, alias))

	store := newTestConfigStore(testStoreOpts{globalDataPath: path, config: &Config{Options: &Options{}}})
	store.workingDir = root
	err := store.SetConfigField(ScopeGlobal, "options.debug", true)
	require.ErrorIs(t, err, ErrConfigHardLink)
	require.Equal(t, contents, mustReadFile(t, path))
	require.Equal(t, contents, mustReadFile(t, alias))
}

func TestUnixCommitPinsPhysicalSymlinkTargetAndReportsRetarget(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.json")
	second := filepath.Join(root, "second.json")
	alias := filepath.Join(root, "rush.json")
	require.NoError(t, os.WriteFile(first, []byte(`{"which":"first"}`), 0o600))
	require.NoError(t, os.WriteFile(second, []byte(`{"which":"second"}`), 0o600))
	if err := os.Symlink(first, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, expected, err := readStableConfigFile(alias)
	require.NoError(t, err)

	configTestHooks.Lock()
	previous := configTestHooks.beforeCommitRename
	configTestHooks.beforeCommitRename = func() {
		_ = os.Remove(alias)
		_ = os.Symlink(second, alias)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeCommitRename = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(alias, normalizeReloadPath(alias), []byte(`{"which":"new"}`), 0o600, expected, 0, false)
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.True(t, outcome.Committed)
	require.False(t, outcome.Reconciled)
	require.ErrorIs(t, outcome, errConfigCommitUncertain)
	require.Equal(t, []byte(`{"which":"new"}`), mustReadFile(t, first))
	require.Equal(t, []byte(`{"which":"second"}`), mustReadFile(t, second))
}

func TestCommitOutcomeIsPublicAndPreservesCause(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o600))
	parentSyncErr := errors.New("injected parent fsync failure")
	configTestHooks.Lock()
	previous := configTestHooks.syncParent
	configTestHooks.syncParent = func(string) error { return parentSyncErr }
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.syncParent = previous
		configTestHooks.Unlock()
	})

	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)
	_, err = commitConfigFile(path, path, []byte(`{"new":true}`), 0o600, expected, 0, false)
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.True(t, outcome.Committed)
	require.True(t, outcome.Reconciled)
	require.Equal(t, filepath.Clean(path), outcome.Path)
	require.ErrorIs(t, outcome, errConfigCommitDurabilityUncertain)
	require.ErrorIs(t, outcome, parentSyncErr)
	require.NotContains(t, err.Error(), `{"new":true}`)
}

func TestUnixCommitSyncsPinnedParentAfterPathSwap(t *testing.T) {
	root := t.TempDir()
	parentPath := filepath.Join(root, "config")
	require.NoError(t, os.Mkdir(parentPath, 0o755))
	path := filepath.Join(parentPath, "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o600))
	parentInfo, err := os.Stat(parentPath)
	require.NoError(t, err)
	wantParent := configFileIdentityOf(parentInfo)
	var syncedParent configFileIdentity

	configTestHooks.Lock()
	previousAfter := configTestHooks.afterCommitRenamePath
	previousSyncFile := configTestHooks.syncParentFile
	configTestHooks.afterCommitRenamePath = func(hookPath string) error {
		if hookPath != path {
			return nil
		}
		if err := os.Rename(parentPath, parentPath+".moved"); err != nil {
			return err
		}
		return os.Mkdir(parentPath, 0o755)
	}
	configTestHooks.syncParentFile = func(file *os.File) error {
		info, statErr := file.Stat()
		if statErr != nil {
			return statErr
		}
		syncedParent = configFileIdentityOf(info)
		return file.Sync()
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.afterCommitRenamePath = previousAfter
		configTestHooks.syncParentFile = previousSyncFile
		configTestHooks.Unlock()
	})

	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)
	_, err = commitConfigFile(path, path, []byte(`{"new":true}`), 0o600, expected, 0, false)
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.True(t, outcome.Committed)
	require.False(t, outcome.Reconciled)
	require.Equal(t, wantParent, syncedParent)
	require.Equal(t, []byte(`{"new":true}`), mustReadFile(t, filepath.Join(parentPath+".moved", "rush.json")))
}
