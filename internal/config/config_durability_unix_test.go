//go:build !windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPParentSyncFailureReconcilesSnapshotButReturnsUncertainty(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "rush.json")
	writeMCPDefinitionState(t, global, "server", MCPConfig{Type: MCPHttp, URL: "http://old.example"})
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{MCP: MCPs{"server": {Type: MCPHttp, URL: "http://old.example"}}},
		globalDataPath: global,
	})
	store.captureStalenessSnapshot([]string{global})
	store.workingDir = root
	parentSyncErr := errors.New("injected parent fsync failure")
	configTestHooks.Lock()
	previous := configTestHooks.syncParent
	configTestHooks.syncParent = func(string) error { return parentSyncErr }
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.syncParent = previous
		configTestHooks.Unlock()
	})

	beforeGeneration := store.Generation()
	result, err := store.PersistMCPConfigResult(ScopeGlobal, "new", MCPConfig{Type: MCPHttp, URL: "http://new.example"})
	require.ErrorIs(t, err, ErrMCPCommitUncertain)
	require.ErrorIs(t, err, errConfigCommitDurabilityUncertain)
	require.True(t, result.NewExists)
	require.Equal(t, "http://new.example", result.NewConfig.URL)
	require.Equal(t, beforeGeneration+1, store.Generation())
	require.False(t, store.ConfigStaleness().Dirty)
	got, ok := store.MCPConfig("new")
	require.True(t, ok)
	require.Equal(t, "http://new.example", got.URL)
	_, diskFingerprint, readErr := readStableConfigFile(global)
	require.NoError(t, readErr)
	require.Equal(t, diskFingerprint, store.loadSnapshot().snapshots[normalizeDiscoveryPath(global)].fingerprint)

	// The caller received an uncertainty and must not blindly retry the add:
	// the visible committed state is already authoritative for the next
	// transaction boundary.
	configTestHooks.Lock()
	configTestHooks.syncParent = previous
	configTestHooks.Unlock()
	_, retryErr := store.PersistMCPConfigResult(ScopeGlobal, "new", MCPConfig{Type: MCPHttp, URL: "http://new.example"})
	require.ErrorIs(t, retryErr, ErrMCPTargetExists)
	require.Equal(t, beforeGeneration+1, store.Generation())
}
