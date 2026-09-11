//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestPersistMCPPreservesMoveFileExDurabilityUncertaintyAfterReconcile(t *testing.T) {
	store, _ := isolatedMCPConfigStore(t)
	path := GlobalConfigData()

	configTestHooks.Lock()
	previous := configTestHooks.moveFileEx
	configTestHooks.moveFileEx = func(from, to *uint16, _ uint32) error {
		require.NoError(t, os.Rename(windows.UTF16PtrToString(from), windows.UTF16PtrToString(to)))
		return windows.ERROR_ACCESS_DENIED
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.moveFileEx = previous
		configTestHooks.Unlock()
	})

	result, err := store.PersistMCPConfigResult(ScopeGlobal, "durability", MCPConfig{
		Type: MCPHttp,
		URL:  "http://durability.example",
	})
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.True(t, outcome.Committed)
	require.True(t, outcome.Reconciled)
	require.ErrorIs(t, err, ErrMCPCommitUncertain)
	require.ErrorIs(t, err, errAtomicWriteCommitted)
	require.ErrorIs(t, err, errConfigCommitDurabilityUncertain)
	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)
	require.True(t, result.NewExists)
	require.Equal(t, "http://durability.example", result.NewConfig.URL)
	require.Equal(t, normalizeReloadPath(path), outcome.Path)

	entries, readDirErr := os.ReadDir(filepath.Dir(path))
	require.NoError(t, readDirErr)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".tmp")
	}
}
