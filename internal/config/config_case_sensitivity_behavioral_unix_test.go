//go:build !windows && !darwin

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigBehavioralCaseSensitivityIsMutationFree(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "Rush.json")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	before := directoryEntryNames(t, root)
	_ = configDirectoryCaseSensitivityByEntries(root)
	require.Equal(t, before, directoryEntryNames(t, root))
}
