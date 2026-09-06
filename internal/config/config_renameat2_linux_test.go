//go:build linux

package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestLinuxRenameNoReplaceFallsBackOnlyForCapabilityErrors(t *testing.T) {
	capabilityErrors := []error{unix.ENOSYS, unix.EINVAL, unix.EOPNOTSUPP}
	for _, capabilityErr := range capabilityErrors {
		capabilityErr := capabilityErr
		t.Run(capabilityErr.Error(), func(t *testing.T) {
			root := t.TempDir()
			tempName := ".rush.json.test.tmp"
			destinationName := "rush.json"
			require.NoError(t, os.WriteFile(filepath.Join(root, tempName), []byte(`{"new":true}`), 0o600))
			dirFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			require.NoError(t, err)
			dir := os.NewFile(uintptr(dirFD), root)
			t.Cleanup(func() { _ = dir.Close() })

			configTestHooks.Lock()
			previous := configTestHooks.renameNoReplace
			configTestHooks.renameNoReplace = func(int, string, int, string) error { return capabilityErr }
			configTestHooks.Unlock()
			t.Cleanup(func() {
				configTestHooks.Lock()
				configTestHooks.renameNoReplace = previous
				configTestHooks.Unlock()
			})

			published, err := renameConfigTempAt(dirFD, tempName, dirFD, destinationName, false)
			require.NoError(t, err)
			require.True(t, published)
			require.Equal(t, []byte(`{"new":true}`), mustReadFile(t, filepath.Join(root, destinationName)))
			_, err = os.Stat(filepath.Join(root, tempName))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestLinuxRenameNoReplaceDoesNotMaskRealErrors(t *testing.T) {
	for _, injectedErr := range []error{unix.EEXIST, unix.EPERM} {
		injectedErr := injectedErr
		t.Run(injectedErr.Error(), func(t *testing.T) {
			root := t.TempDir()
			tempName := ".rush.json.test.tmp"
			destinationName := "rush.json"
			require.NoError(t, os.WriteFile(filepath.Join(root, tempName), []byte(`{"new":true}`), 0o600))
			dirFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			require.NoError(t, err)
			dir := os.NewFile(uintptr(dirFD), root)
			t.Cleanup(func() { _ = dir.Close() })

			configTestHooks.Lock()
			previous := configTestHooks.renameNoReplace
			configTestHooks.renameNoReplace = func(int, string, int, string) error { return injectedErr }
			configTestHooks.Unlock()
			t.Cleanup(func() {
				configTestHooks.Lock()
				configTestHooks.renameNoReplace = previous
				configTestHooks.Unlock()
			})

			published, err := renameConfigTempAt(dirFD, tempName, dirFD, destinationName, false)
			require.False(t, published)
			require.True(t, errors.Is(err, injectedErr))
			require.Equal(t, []byte(`{"new":true}`), mustReadFile(t, filepath.Join(root, tempName)))
		})
	}
}
