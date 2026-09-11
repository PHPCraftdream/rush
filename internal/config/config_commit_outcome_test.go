package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPReconcileReturnsReconciledPublicCommitOutcome(t *testing.T) {
	store, _ := isolatedMCPConfigStore(t)
	path := GlobalConfigData()

	configTestHooks.Lock()
	previousAfter := configTestHooks.afterCommitRenamePath
	previousReconcile := configTestHooks.beforeMCPReconcile
	var removed, restored bool
	var restoreErr error
	configTestHooks.afterCommitRenamePath = func(hookPath string) error {
		removed = true
		return os.Remove(hookPath)
	}
	configTestHooks.beforeMCPReconcile = func(reconcilePath string, reconcileData []byte) {
		restored = true
		restoreErr = os.WriteFile(reconcilePath, reconcileData, 0o600)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.afterCommitRenamePath = previousAfter
		configTestHooks.beforeMCPReconcile = previousReconcile
		configTestHooks.Unlock()
	})

	result, err := store.PersistMCPConfigResult(ScopeGlobal, "reconciled", MCPConfig{
		Type: MCPHttp,
		URL:  "http://reconciled.example",
	})
	require.True(t, removed, "post-rename fault injection did not run")
	require.True(t, restored, "MCP reconciliation did not run")
	require.NoError(t, restoreErr)
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.True(t, outcome.Committed)
	require.True(t, outcome.Reconciled)
	require.Equal(t, filepath.Clean(path), outcome.Path)
	require.ErrorIs(t, outcome, errConfigCommitUncertain)
	require.ErrorIs(t, outcome, errConfigCommitCommitted)
	require.ErrorIs(t, err, ErrMCPCommitUncertain)
	require.True(t, result.NewExists)
	require.Equal(t, "http://reconciled.example", result.NewConfig.URL)
	require.Equal(t, []byte("{\n  \"mcp\": {\n    \"reconciled\": {\n      \"type\": \"http\",\n      \"url\": \"http://reconciled.example\"\n    }\n  }\n}\n"), mustReadFile(t, path))
}
