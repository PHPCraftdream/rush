//go:build linux

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigLinuxCaseVariantHardlinkDoesNotProveCaseInsensitivity(t *testing.T) {
	root := t.TempDir()
	left := filepath.Join(root, "Rush.json")
	right := filepath.Join(root, "rush.json")
	require.NoError(t, os.WriteFile(left, nil, 0o600))
	if err := os.Link(left, right); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	if len(entries) != 2 {
		t.Skip("filesystem resolves case variants to one directory entry")
	}

	_, folded := configPlatformCaseFoldLeaf(root, filepath.Base(left))
	require.False(t, folded)
	_, folded = configPlatformCaseFoldLeaf(root, filepath.Base(right))
	require.False(t, folded)
}
