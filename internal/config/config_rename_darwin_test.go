//go:build darwin

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestDarwinRenameConfigTempAtUsesDescriptorRelativeNoReplace(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	destinationDir := filepath.Join(root, "destination")
	require.NoError(t, os.Mkdir(sourceDir, 0o700))
	require.NoError(t, os.Mkdir(destinationDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "temp"), []byte("new"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(destinationDir, "config"), []byte("old"), 0o600))

	sourceFD, err := unix.Open(sourceDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	source := os.NewFile(uintptr(sourceFD), sourceDir)
	t.Cleanup(func() { _ = source.Close() })
	destinationFD, err := unix.Open(destinationDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	destination := os.NewFile(uintptr(destinationFD), destinationDir)
	t.Cleanup(func() { _ = destination.Close() })

	published, err := renameConfigTempAt(sourceFD, "temp", destinationFD, "config", false)
	require.ErrorIs(t, err, unix.EEXIST)
	require.False(t, published)
	require.Equal(t, []byte("old"), mustReadFile(t, filepath.Join(destinationDir, "config")))
	require.Equal(t, []byte("new"), mustReadFile(t, filepath.Join(sourceDir, "temp")))

	published, err = renameConfigTempAt(sourceFD, "temp", destinationFD, "published", false)
	require.NoError(t, err)
	require.True(t, published)
	require.Equal(t, []byte("new"), mustReadFile(t, filepath.Join(destinationDir, "published")))
}

func TestDarwinRenameNoReplaceFallsBackOnlyForCapabilityErrors(t *testing.T) {
	for _, capabilityErr := range []error{unix.ENOSYS, unix.EINVAL, unix.ENOTSUP, unix.EOPNOTSUPP} {
		capabilityErr := capabilityErr
		t.Run(capabilityErr.Error(), func(t *testing.T) {
			root := t.TempDir()
			tempName := "temp"
			destinationName := "published"
			require.NoError(t, os.WriteFile(filepath.Join(root, tempName), []byte("new"), 0o600))
			dirFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			require.NoError(t, err)
			dir := os.NewFile(uintptr(dirFD), root)
			t.Cleanup(func() { _ = dir.Close() })

			configTestHooks.Lock()
			previous := configTestHooks.renameatxNp
			configTestHooks.renameatxNp = func(int, string, int, string, uint32) error { return capabilityErr }
			configTestHooks.Unlock()
			t.Cleanup(func() {
				configTestHooks.Lock()
				configTestHooks.renameatxNp = previous
				configTestHooks.Unlock()
			})

			published, err := renameConfigTempAt(dirFD, tempName, dirFD, destinationName, false)
			require.NoError(t, err)
			require.True(t, published)
			require.Equal(t, []byte("new"), mustReadFile(t, filepath.Join(root, destinationName)))
		})
	}
}

func TestDarwinRenameNoReplaceDoesNotMaskRealErrors(t *testing.T) {
	for _, injectedErr := range []error{unix.EEXIST, unix.EPERM} {
		injectedErr := injectedErr
		t.Run(injectedErr.Error(), func(t *testing.T) {
			root := t.TempDir()
			tempName := "temp"
			destinationName := "published"
			require.NoError(t, os.WriteFile(filepath.Join(root, tempName), []byte("new"), 0o600))
			dirFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			require.NoError(t, err)
			dir := os.NewFile(uintptr(dirFD), root)
			t.Cleanup(func() { _ = dir.Close() })

			var calls int
			configTestHooks.Lock()
			previous := configTestHooks.renameatxNp
			configTestHooks.renameatxNp = func(int, string, int, string, uint32) error {
				calls++
				return injectedErr
			}
			configTestHooks.Unlock()
			t.Cleanup(func() {
				configTestHooks.Lock()
				configTestHooks.renameatxNp = previous
				configTestHooks.Unlock()
			})

			published, err := renameConfigTempAt(dirFD, tempName, dirFD, destinationName, false)
			require.ErrorIs(t, err, injectedErr)
			require.False(t, published)
			require.Equal(t, 1, calls)
			require.Equal(t, []byte("new"), mustReadFile(t, filepath.Join(root, tempName)))
			_, statErr := os.Stat(filepath.Join(root, destinationName))
			require.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
}

func TestDarwinRenameNoReplaceSyscallSeamUsesRenameExcl(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	destinationDir := filepath.Join(root, "destination")
	require.NoError(t, os.Mkdir(sourceDir, 0o700))
	require.NoError(t, os.Mkdir(destinationDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "temp"), []byte("new"), 0o600))
	sourceFD, err := unix.Open(sourceDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	source := os.NewFile(uintptr(sourceFD), sourceDir)
	t.Cleanup(func() { _ = source.Close() })
	destinationFD, err := unix.Open(destinationDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	destination := os.NewFile(uintptr(destinationFD), destinationDir)
	t.Cleanup(func() { _ = destination.Close() })

	var gotOldDirFD, gotNewDirFD int
	var gotOldName, gotNewName string
	var gotFlags uint32
	configTestHooks.Lock()
	previous := configTestHooks.renameatxNp
	configTestHooks.renameatxNp = func(oldDirFD int, oldName string, newDirFD int, newName string, flags uint32) error {
		gotOldDirFD, gotOldName = oldDirFD, oldName
		gotNewDirFD, gotNewName, gotFlags = newDirFD, newName, flags
		return unix.Renameat(oldDirFD, oldName, newDirFD, newName)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.renameatxNp = previous
		configTestHooks.Unlock()
	})

	published, err := renameConfigTempAt(sourceFD, "temp", destinationFD, "config", false)
	require.NoError(t, err)
	require.True(t, published)
	require.Equal(t, sourceFD, gotOldDirFD)
	require.Equal(t, destinationFD, gotNewDirFD)
	require.Equal(t, "temp", gotOldName)
	require.Equal(t, "config", gotNewName)
	require.Equal(t, uint32(unix.RENAME_EXCL), gotFlags)
}

func TestDarwinRenameNoReplaceUsesPinnedDirectoriesAfterPathSwap(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	destinationDir := filepath.Join(root, "destination")
	require.NoError(t, os.Mkdir(sourceDir, 0o700))
	require.NoError(t, os.Mkdir(destinationDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "temp"), []byte("new"), 0o600))
	sourceFD, err := unix.Open(sourceDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	source := os.NewFile(uintptr(sourceFD), sourceDir)
	t.Cleanup(func() { _ = source.Close() })
	destinationFD, err := unix.Open(destinationDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	destination := os.NewFile(uintptr(destinationFD), destinationDir)
	t.Cleanup(func() { _ = destination.Close() })

	require.NoError(t, os.Rename(sourceDir, sourceDir+".moved"))
	require.NoError(t, os.Mkdir(sourceDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "temp"), []byte("source decoy"), 0o600))
	require.NoError(t, os.Rename(destinationDir, destinationDir+".moved"))
	require.NoError(t, os.Mkdir(destinationDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(destinationDir, "config"), []byte("destination decoy"), 0o600))

	published, err := renameConfigTempAt(sourceFD, "temp", destinationFD, "config", false)
	require.NoError(t, err)
	require.True(t, published)
	require.Equal(t, []byte("new"), mustReadFile(t, filepath.Join(destinationDir+".moved", "config")))
	require.Equal(t, []byte("source decoy"), mustReadFile(t, filepath.Join(sourceDir, "temp")))
	require.Equal(t, []byte("destination decoy"), mustReadFile(t, filepath.Join(destinationDir, "config")))
	_, err = os.Stat(filepath.Join(sourceDir+".moved", "temp"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
