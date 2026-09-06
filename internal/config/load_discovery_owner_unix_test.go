//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/stretchr/testify/require"
)

func TestLookupConfigCandidatesRejectsForeignOwnedConfig(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"options":{"debug":true}}`), 0o600))

	owner, err := fsext.Owner(root)
	require.NoError(t, err)
	foreignOwner := 0
	if owner == foreignOwner {
		foreignOwner = 1
	}
	if err := os.Chown(path, foreignOwner, -1); err != nil {
		t.Skipf("changing test-file ownership unavailable: %v", err)
	}
	worktreeRootCache.Store(canonicalConfigPath(root), "")
	t.Cleanup(func() { worktreeRootCache.Delete(canonicalConfigPath(root)) })

	require.NotContains(t, lookupConfigCandidates(root), canonicalConfigPath(path))
}
