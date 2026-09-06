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
