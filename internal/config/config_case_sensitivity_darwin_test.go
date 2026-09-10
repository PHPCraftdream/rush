//go:build darwin

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDarwinCaseSensitivityMatchesFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	before := directoryEntryNames(t, dir)
	sensitivity := configPlatformCaseSensitivity(dir)
	require.Equal(t, before, directoryEntryNames(t, dir))
	require.NotEqual(t, configCaseSensitivityUnknown, sensitivity)

	path := filepath.Join(dir, "CaseProbe")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	original, err := os.Stat(path)
	require.NoError(t, err)
	alternate, err := os.Stat(filepath.Join(dir, "caseprobe"))
	if os.IsNotExist(err) {
		require.Equal(t, configCaseSensitivitySensitive, sensitivity)
	} else {
		require.NoError(t, err)
		require.True(t, os.SameFile(original, alternate))
		require.Equal(t, configCaseSensitivityInsensitive, sensitivity)
	}
	require.Equal(t, configCaseSensitivityUnknown, configPlatformCaseSensitivity(filepath.Join(dir, "missing")))
	require.Equal(t, configCaseSensitivityUnknown, configPlatformCaseSensitivity("bad\x00path"))
}
