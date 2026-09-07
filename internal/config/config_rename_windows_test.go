//go:build windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestRenameConfigTempUsesWriteThroughAndOptionalReplacement(t *testing.T) {
	for _, test := range []struct {
		name      string
		replace   bool
		wantFlags uint32
	}{
		{name: "no replace", wantFlags: windows.MOVEFILE_WRITE_THROUGH},
		{name: "replace", replace: true, wantFlags: windows.MOVEFILE_WRITE_THROUGH | windows.MOVEFILE_REPLACE_EXISTING},
	} {
		t.Run(test.name, func(t *testing.T) {
			apiErr := errors.New("injected MoveFileEx failure")
			var gotFlags uint32
			configTestHooks.Lock()
			previous := configTestHooks.moveFileEx
			configTestHooks.moveFileEx = func(_, _ *uint16, flags uint32) error {
				gotFlags = flags
				return apiErr
			}
			configTestHooks.Unlock()
			t.Cleanup(func() {
				configTestHooks.Lock()
				configTestHooks.moveFileEx = previous
				configTestHooks.Unlock()
			})

			err := renameConfigTemp("source", "destination", test.replace)
			require.ErrorIs(t, err, apiErr)
			require.Equal(t, test.wantFlags, gotFlags)
		})
	}
}

func TestWindowsCommitMoveFileExFailureAfterPublicationReturnsCommitOutcome(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	data := []byte(`{"new":true}`)
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	apiErr := windows.ERROR_ACCESS_DENIED
	configTestHooks.Lock()
	previous := configTestHooks.moveFileEx
	configTestHooks.moveFileEx = func(from, to *uint16, _ uint32) error {
		fromPath := windows.UTF16PtrToString(from)
		toPath := windows.UTF16PtrToString(to)
		if renameErr := os.Rename(fromPath, toPath); renameErr != nil {
			return renameErr
		}
		return apiErr
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.moveFileEx = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, data, 0o600, expected, -1, false)
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.True(t, outcome.Committed)
	require.True(t, outcome.Reconciled)
	require.ErrorIs(t, outcome, errConfigCommitCommitted)
	require.ErrorIs(t, outcome, errAtomicWriteCommitted)
	require.ErrorIs(t, outcome, errConfigCommitDurabilityUncertain)
	require.ErrorIs(t, outcome, apiErr)
	require.Equal(t, data, mustReadFile(t, path))
}

func TestWindowsCommitMoveFileExAccessDeniedBeforePublicationLeavesIdenticalDestination(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	data := []byte(`{"same":true}`)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	configTestHooks.Lock()
	previous := configTestHooks.moveFileEx
	configTestHooks.moveFileEx = func(_, _ *uint16, _ uint32) error {
		return windows.ERROR_ACCESS_DENIED
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.moveFileEx = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, data, 0o600, expected, -1, false)
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.False(t, outcome.Committed)
	require.False(t, outcome.Reconciled)
	require.ErrorIs(t, outcome, windows.ERROR_ACCESS_DENIED)
	require.Equal(t, data, mustReadFile(t, path))

	entries, readDirErr := os.ReadDir(root)
	require.NoError(t, readDirErr)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".tmp")
	}
}

func TestWindowsCommitHandleRetryRejectsInPlaceDestinationRewrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	oldData := []byte(`{"old":true}`)
	changedData := []byte(`{"writer":true}`)
	newData := []byte(`{"new":true}`)
	require.NoError(t, os.WriteFile(path, oldData, 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	attempts := 0
	configTestHooks.Lock()
	previous := configTestHooks.setFileInformation
	configTestHooks.setFileInformation = func(_ uintptr, class uint32, _ *byte, _ uint32) error {
		if class == windows.FileRenameInfoEx || class == windows.FileRenameInfo {
			attempts++
			if attempts == 1 {
				require.NoError(t, os.WriteFile(path, changedData, 0o600))
				return windows.ERROR_ACCESS_DENIED
			}
		}
		return nil
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.setFileInformation = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, newData, 0o600, expected, -1, false)
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.False(t, outcome.Committed)
	require.ErrorIs(t, outcome, errConfigCommitVerification)
	require.Equal(t, 1, attempts)
	require.Equal(t, changedData, mustReadFile(t, path))
}

func TestWindowsCommitHandleNoReplaceRetryRejectsDestinationCreation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	newData := []byte(`{"new":true}`)
	_, expected, err := readStableConfigFile(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	attempts := 0
	configTestHooks.Lock()
	previous := configTestHooks.setFileInformation
	configTestHooks.setFileInformation = func(_ uintptr, class uint32, _ *byte, _ uint32) error {
		if class == windows.FileRenameInfoEx || class == windows.FileRenameInfo {
			attempts++
			if attempts == 1 {
				require.NoError(t, os.WriteFile(path, []byte(`{"writer":true}`), 0o600))
				return windows.ERROR_ACCESS_DENIED
			}
		}
		return nil
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.setFileInformation = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, newData, 0o600, expected, -1, false)
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.False(t, outcome.Committed)
	require.ErrorIs(t, outcome, errConfigCommitVerification)
	require.Equal(t, 1, attempts)
	require.Equal(t, []byte(`{"writer":true}`), mustReadFile(t, path))
}

func TestWindowsCommitHandleRetryUnchangedDestinationSucceeds(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	oldData := []byte(`{"old":true}`)
	newData := []byte(`{"new":true}`)
	require.NoError(t, os.WriteFile(path, oldData, 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	attempts := 0
	configTestHooks.Lock()
	previous := configTestHooks.setFileInformation
	configTestHooks.setFileInformation = func(_ uintptr, class uint32, _ *byte, _ uint32) error {
		if class == windows.FileRenameInfoEx || class == windows.FileRenameInfo {
			attempts++
			if attempts == 1 {
				configTestHooks.Lock()
				configTestHooks.setFileInformation = nil
				configTestHooks.Unlock()
				return windows.ERROR_ACCESS_DENIED
			}
		}
		return nil
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.setFileInformation = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, newData, 0o600, expected, -1, false)
	require.NoError(t, err)
	require.Equal(t, 1, attempts)
	require.Equal(t, newData, mustReadFile(t, path))
}

func TestWindowsCommitHandleUnsupportedFallbackRejectsDestinationRewrite(t *testing.T) {
	for _, unsupported := range []struct {
		name string
		err  error
	}{
		{name: "invalid parameter", err: windows.ERROR_INVALID_PARAMETER},
		{name: "not supported", err: windows.ERROR_NOT_SUPPORTED},
	} {
		t.Run(unsupported.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "rush.json")
			oldData := []byte(`{"old":true}`)
			writerData := []byte(`{"writer":true}`)
			newData := []byte(`{"new":true}`)
			require.NoError(t, os.WriteFile(path, oldData, 0o600))
			_, expected, err := readStableConfigFile(path)
			require.NoError(t, err)

			calls := 0
			configTestHooks.Lock()
			previousRename := configTestHooks.setFileInformation
			previousBeforeRename := configTestHooks.beforeCommitRename
			configTestHooks.beforeCommitRename = func() {}
			configTestHooks.setFileInformation = func(_ uintptr, class uint32, _ *byte, _ uint32) error {
				if class == windows.FileRenameInfoEx {
					calls++
					require.NoError(t, os.WriteFile(path, writerData, 0o600))
					return unsupported.err
				}
				if class == windows.FileRenameInfo {
					calls++
					t.Fatal("legacy rename was attempted after destination changed")
				}
				return nil
			}
			configTestHooks.Unlock()
			t.Cleanup(func() {
				configTestHooks.Lock()
				configTestHooks.setFileInformation = previousRename
				configTestHooks.beforeCommitRename = previousBeforeRename
				configTestHooks.Unlock()
			})

			_, err = commitConfigFile(path, path, newData, 0o600, expected, -1, false)
			var outcome *CommitOutcome
			require.ErrorAs(t, err, &outcome)
			require.False(t, outcome.Committed)
			require.ErrorIs(t, outcome, errConfigCommitVerification)
			require.Equal(t, 1, calls)
			require.Equal(t, writerData, mustReadFile(t, path))
		})
	}
}

func TestWindowsCommitHandleUnsupportedFallbackUsesLegacyAfterFreshProof(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	oldData := []byte(`{"old":true}`)
	newData := []byte(`{"new":true}`)
	require.NoError(t, os.WriteFile(path, oldData, 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	var stagedPath string
	calls := 0
	configTestHooks.Lock()
	previousBeforeRename := configTestHooks.beforeCommitRename
	previousRename := configTestHooks.setFileInformation
	configTestHooks.beforeCommitRename = func() {
		entries, readDirErr := os.ReadDir(root)
		require.NoError(t, readDirErr)
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".tmp") {
				stagedPath = filepath.Join(root, entry.Name())
				break
			}
		}
		require.NotEmpty(t, stagedPath)
	}
	configTestHooks.setFileInformation = func(_ uintptr, class uint32, _ *byte, _ uint32) error {
		if class == windows.FileRenameInfoEx {
			calls++
			return windows.ERROR_NOT_SUPPORTED
		}
		if class == windows.FileRenameInfo {
			calls++
			return os.Rename(stagedPath, path)
		}
		return nil
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeCommitRename = previousBeforeRename
		configTestHooks.setFileInformation = previousRename
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, newData, 0o600, expected, -1, false)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, newData, mustReadFile(t, path))
}

func TestWindowsParentSyncIsAProductionNoOp(t *testing.T) {
	require.NoError(t, syncConfigParentOnDisk(filepath.Join(t.TempDir(), "does-not-exist")))
}

func TestWindowsAPIPathUsesExtendedLengthForLongDriveAndUNCPaths(t *testing.T) {
	drivePath := filepath.Join(t.TempDir(), strings.Repeat("long-directory\\", 24), "rush.json")
	require.Greater(t, len(drivePath), 260)
	driveAPIPath, err := windowsConfigAPIPath(drivePath)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(driveAPIPath, `\\?\`))

	uncPath := `\\server\share\` + strings.Repeat("long-directory\\", 24) + "rush.json"
	uncAPIPath, err := windowsConfigAPIPath(uncPath)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(uncAPIPath, `\\?\UNC\`))
}

func TestWindowsStageSupportsLongPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, strings.Repeat("long-directory\\", 24), "rush.json")
	require.Greater(t, len(path), 260)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	staged, err := stageConfigFileHandle(path, []byte(`{"long":true}`), 0o600)
	require.NoError(t, err)
	require.NoError(t, deleteWindowsConfigHandle(staged.file))
	require.NoError(t, staged.file.Close())
}

func TestWindowsCommitSupportsLongPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, strings.Repeat("long-directory\\", 24), "rush.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	oldData := []byte(`{"old":true}`)
	newData := []byte(`{"new":true}`)
	require.NoError(t, os.WriteFile(path, oldData, 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	_, err = commitConfigFile(path, path, newData, 0o600, expected, -1, false)
	require.NoError(t, err)
	require.Equal(t, newData, mustReadFile(t, path))
}

func TestWindowsAbsentFingerprintPinsParentIdentity(t *testing.T) {
	root := t.TempDir()
	parentPath := filepath.Join(root, "config")
	path := filepath.Join(parentPath, "rush.json")
	require.NoError(t, os.Mkdir(parentPath, 0o755))

	_, expected, err := readStableConfigFile(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.True(t, expected.parentIdentity.valid)

	movedPath := filepath.Join(root, "config.moved")
	require.NoError(t, os.Rename(parentPath, movedPath))
	require.NoError(t, os.Mkdir(parentPath, 0o755))
	_, actual, err := readStableConfigFile(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NotEqual(t, expected.parentIdentity, actual.parentIdentity)
}

func TestRenameConfigTempHandleUsesPinnedParentAndRelativeName(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.tmp")
	require.NoError(t, os.WriteFile(sourcePath, []byte("source"), 0o600))
	source, err := openWindowsConfigHandle(sourcePath, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE, windows.FILE_FLAG_OPEN_REPARSE_POINT)
	require.NoError(t, err)
	defer source.Close()
	parent, err := openWindowsConfigHandle(root, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)
	require.NoError(t, err)
	defer parent.Close()

	var gotClass uint32
	var gotRoot windows.Handle
	var gotName string
	configTestHooks.Lock()
	previous := configTestHooks.setFileInformation
	configTestHooks.setFileInformation = func(_ uintptr, class uint32, buffer *byte, length uint32) error {
		gotClass = class
		info := (*windowsFileRenameInfo)(unsafe.Pointer(buffer))
		gotRoot = info.RootDirectory
		name := unsafe.Slice(&info.FileName[0], int(info.FileNameLength/2))
		gotName = windows.UTF16ToString(name)
		require.Equal(t, uint32(unsafe.Offsetof(windowsFileRenameInfo{}.FileName))+info.FileNameLength, length)
		return nil
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.setFileInformation = previous
		configTestHooks.Unlock()
	})

	require.NoError(t, renameConfigTempHandle(source, sourcePath, parent, "published.json", true, reloadFileFingerprint{}, -1, false, sourcePath))
	require.Equal(t, uint32(windows.FileRenameInfoEx), gotClass)
	require.Equal(t, windows.Handle(parent.Fd()), gotRoot)
	require.Equal(t, "published.json", gotName)
	require.NotContains(t, gotName, string(filepath.Separator))
}

func TestWindowsRenameRetryClosesExistingNoReplaceTarget(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.tmp")
	targetPath := filepath.Join(root, "published.json")
	require.NoError(t, os.WriteFile(sourcePath, []byte("source"), 0o600))
	require.NoError(t, os.WriteFile(targetPath, []byte("target"), 0o600))
	source, err := openWindowsConfigHandle(sourcePath, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE, windows.FILE_FLAG_OPEN_REPARSE_POINT)
	require.NoError(t, err)
	defer source.Close()
	parent, err := openWindowsConfigHandle(root, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)
	require.NoError(t, err)
	defer parent.Close()

	configTestHooks.Lock()
	previous := configTestHooks.setFileInformation
	configTestHooks.setFileInformation = func(_ uintptr, class uint32, _ *byte, _ uint32) error {
		if class == windows.FileRenameInfoEx {
			return windows.ERROR_INVALID_PARAMETER
		}
		return nil
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.setFileInformation = previous
		configTestHooks.Unlock()
	})

	for range 32 {
		err = renameConfigTempHandle(source, sourcePath, parent, filepath.Base(targetPath), false, reloadFileFingerprint{}, -1, false, sourcePath)
		require.ErrorIs(t, err, errConfigCommitVerification)
		require.NoError(t, os.Remove(targetPath), "retry verification must close an existing no-replace target")
		require.NoError(t, os.WriteFile(targetPath, []byte("target"), 0o600))
	}
}

func TestWindowsCommitRejectsPinnedParentAfterPathMove(t *testing.T) {
	root := t.TempDir()
	parentPath := filepath.Join(root, "config")
	movedParentPath := filepath.Join(root, "config.moved")
	path := filepath.Join(parentPath, "rush.json")
	data := []byte(`{"new":true}`)
	decoy := []byte(`{"decoy":true}`)
	require.NoError(t, os.Mkdir(parentPath, 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	configTestHooks.Lock()
	previousCheck := configTestHooks.beforeCommitCheck
	previousSync := configTestHooks.syncParentFD
	configTestHooks.beforeCommitCheck = func() {
		require.NoError(t, os.Rename(parentPath, movedParentPath))
		require.NoError(t, os.Mkdir(parentPath, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(parentPath, "rush.json"), decoy, 0o600))
	}
	configTestHooks.syncParentFD = func(int) error { return nil }
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeCommitCheck = previousCheck
		configTestHooks.syncParentFD = previousSync
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, data, 0o600, expected, -1, false)
	require.ErrorIs(t, err, errConfigCommitVerification)
	require.Equal(t, []byte(`{"old":true}`), mustReadFile(t, filepath.Join(movedParentPath, "rush.json")))
	require.Equal(t, decoy, mustReadFile(t, path))
}
