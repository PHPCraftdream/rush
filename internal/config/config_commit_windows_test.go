//go:build windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func renameWithNativeLegacyInfo(fd uintptr, buffer *byte, length uint32) error {
	info := (*windowsFileRenameInfo)(unsafe.Pointer(buffer))
	info.Flags = 1
	var status windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(
		windows.Handle(fd), &status, buffer, length, windows.FileRenameInformation,
	)
}

func TestWindowsCommitHandleRenameRetriesTransientAccessDenied(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	data := []byte(`{"new":true}`)
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	var attempts atomic.Int32
	configTestHooks.Lock()
	previous := configTestHooks.setFileInformation
	configTestHooks.setFileInformation = func(fd uintptr, class uint32, buffer *byte, length uint32) error {
		if class == windows.FileRenameInfoEx && attempts.Add(1) == 1 {
			return windows.ERROR_ACCESS_DENIED
		}
		if class == windows.FileRenameInfoEx {
			return renameWithNativeLegacyInfo(fd, buffer, length)
		}
		return windows.SetFileInformationByHandle(windows.Handle(fd), class, buffer, length)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.setFileInformation = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, data, 0o600, expected, -1, false)
	require.NoError(t, err)
	require.GreaterOrEqual(t, attempts.Load(), int32(2))
	require.Equal(t, data, mustReadFile(t, path))
}

func TestWindowsCommitHandleRenameRetriesEightTransientAccessDenied(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	oldData := []byte(`{"old":true}`)
	newData := []byte(`{"new":true}`)
	require.NoError(t, os.WriteFile(path, oldData, 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	const transientFailures = 8
	var attempts atomic.Int32
	configTestHooks.Lock()
	previous := configTestHooks.setFileInformation
	configTestHooks.setFileInformation = func(fd uintptr, class uint32, buffer *byte, length uint32) error {
		if class == windows.FileRenameInfoEx {
			attempt := attempts.Add(1)
			if attempt <= transientFailures {
				current, fingerprint, readErr := readStableConfigFile(path)
				require.NoError(t, readErr)
				require.Equal(t, oldData, current)
				require.Equal(t, expected.identity, fingerprint.identity)
				return windows.ERROR_ACCESS_DENIED
			}
			return renameWithNativeLegacyInfo(fd, buffer, length)
		}
		return windows.SetFileInformationByHandle(windows.Handle(fd), class, buffer, length)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.setFileInformation = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, newData, 0o600, expected, -1, false)
	require.NoError(t, err)
	require.Equal(t, int32(transientFailures+1), attempts.Load())
	require.Equal(t, newData, mustReadFile(t, path))
}

func TestWindowsCommitHandleRenameDoesNotRetryAfterAmbiguity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	data := []byte(`{"new":true}`)
	decoy := []byte(`{"decoy":true}`)
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	var attempts atomic.Int32
	configTestHooks.Lock()
	previous := configTestHooks.setFileInformation
	configTestHooks.setFileInformation = func(fd uintptr, class uint32, buffer *byte, length uint32) error {
		if class == windows.FileRenameInfoEx && attempts.Add(1) == 1 {
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.WriteFile(path, decoy, 0o600))
			return windows.ERROR_ACCESS_DENIED
		}
		return windows.SetFileInformationByHandle(windows.Handle(fd), class, buffer, length)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.setFileInformation = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, data, 0o600, expected, -1, false)
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.False(t, outcome.Committed)
	require.Equal(t, int32(1), attempts.Load())
	require.Equal(t, decoy, mustReadFile(t, path))
}

func TestWindowsCommitHandleRenameDoesNotRetryAfterPublication(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	data := []byte(`{"new":true}`)
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	var attempts atomic.Int32
	configTestHooks.Lock()
	previous := configTestHooks.setFileInformation
	configTestHooks.setFileInformation = func(fd uintptr, class uint32, buffer *byte, length uint32) error {
		if class == windows.FileRenameInfoEx && attempts.Add(1) == 1 {
			require.NoError(t, renameWithNativeLegacyInfo(fd, buffer, length))
			return windows.ERROR_ACCESS_DENIED
		}
		if class == windows.FileRenameInfoEx {
			return renameWithNativeLegacyInfo(fd, buffer, length)
		}
		return windows.SetFileInformationByHandle(windows.Handle(fd), class, buffer, length)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.setFileInformation = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, data, 0o600, expected, -1, false)
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.True(t, outcome.Committed)
	require.Equal(t, int32(1), attempts.Load())
	require.Equal(t, data, mustReadFile(t, path))
}

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
	require.Contains(t, string(targetData), `"debug":true`)
	require.Contains(t, string(targetData), "alias.example")
	aliasInfo, err := os.Lstat(alias)
	require.NoError(t, err)
	require.NotEqual(t, 0, aliasInfo.Mode()&os.ModeSymlink)
	require.Equal(t, targetData, mustReadFile(t, alias))
}
