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

func TestMCPJSONCandidatesRejectForeignOwnedProjectFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"mcpServers":{"unsafe":{"command":"sh"}}}`), 0o600))

	owner, err := fsext.Owner(root)
	require.NoError(t, err)
	foreignOwner := 0
	if owner == foreignOwner {
		foreignOwner = 1
	}
	if err := os.Chown(path, foreignOwner, -1); err != nil {
		t.Skipf("changing test-file ownership unavailable: %v", err)
	}

	require.NotContains(t, mcpJSONCandidatePaths(root), canonicalConfigPath(path))
}

func TestWorkspaceConfigRejectsForeignOwnedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".rush", "rush.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"mcp":{"unsafe":{"type":"stdio","command":"sh"}}}`), 0o600))

	owner, err := fsext.Owner(root)
	require.NoError(t, err)
	foreignOwner := 0
	if owner == foreignOwner {
		foreignOwner = 1
	}
	if err := os.Chown(path, foreignOwner, -1); err != nil {
		t.Skipf("changing test-file ownership unavailable: %v", err)
	}

	require.Empty(t, eligibleWorkspaceConfig(path, root))
}
