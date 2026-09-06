//go:build windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWindowsCommitExpectedAbsentRejectsFileCreatedBeforeRename(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	_, expected, err := readStableConfigFile(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	var once sync.Once
	configTestHooks.Lock()
	previous := configTestHooks.beforeCommitCheck
	configTestHooks.beforeCommitCheck = func() {
		once.Do(func() {
			require.NoError(t, os.WriteFile(path, []byte(`{"already":"there"}`), 0o600))
		})
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeCommitCheck = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, []byte(`{"new":"value"}`), 0o600, expected, -1, false)
	require.Error(t, err)
	require.True(t, errors.Is(err, errConfigCommitVerification))
	require.Equal(t, []byte(`{"already":"there"}`), mustReadFile(t, path))
}

func TestWindowsCommitExpectedAbsentNoReplaceClosesCheckCommitRace(t *testing.T) {
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

	_, err = commitConfigFile(path, path, []byte(`{"winner":"ours"}`), 0o600, expected, -1, false)
	require.ErrorIs(t, err, errConfigCommitVerification)
	require.Equal(t, []byte(`{"winner":"other"}`), mustReadFile(t, path))
}

func TestWindowsGenericConfigMutationRejectsHardLink(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	alias := filepath.Join(root, "alias.json")
	contents := []byte(`{"options":{"debug":false}}`)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	if err := os.Link(path, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	_, fingerprint, readErr := readStableConfigFile(path)
	require.NoError(t, readErr)
	require.Greater(t, fingerprint.nlink, uint64(1))

	store := newTestConfigStore(testStoreOpts{globalDataPath: path, config: &Config{Options: &Options{}}})
	store.workingDir = root
	err := store.SetConfigField(ScopeGlobal, "options.debug", true)
	require.ErrorIs(t, err, ErrConfigHardLink)
	require.Equal(t, contents, mustReadFile(t, path))
	require.Equal(t, contents, mustReadFile(t, alias))
}

func TestWindowsConfigSymlinkAliasInDifferentDirectoryPublishesToTarget(t *testing.T) {
	root := t.TempDir()
	targetDir := filepath.Join(root, "target")
	aliasDir := filepath.Join(root, "alias")
	target := filepath.Join(targetDir, "rush.json")
	alias := filepath.Join(aliasDir, "rush.json")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.MkdirAll(aliasDir, 0o755))
	contents := []byte(`{"options":{"debug":false}}`)
	require.NoError(t, os.WriteFile(target, contents, 0o600))
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{Options: &Options{}},
		globalDataPath: alias,
	})
	store.workingDir = root
	require.NoError(t, store.SetConfigField(ScopeGlobal, "options.debug", true))
	require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "alias-test", MCPConfig{
		Type: MCPHttp,
		URL:  "http://alias.example",
	}))

	targetData := mustReadFile(t, target)
	require.Contains(t, string(targetData), `"debug": true`)
	require.Contains(t, string(targetData), "alias.example")
	aliasInfo, err := os.Lstat(alias)
	require.NoError(t, err)
	require.NotEqual(t, 0, aliasInfo.Mode()&os.ModeSymlink)
	require.Equal(t, targetData, mustReadFile(t, alias))
}
