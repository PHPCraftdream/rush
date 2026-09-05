package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigStoreReloadUnstableDiskStopsAtRetryBudget(t *testing.T) {
	store, root := isolatedMCPConfigStore(t)
	configPath := filepath.Join(root, "rush.json")
	writeReloadAttemptConfig(t, configPath, 0)
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	oldConfig, oldGeneration := store.Snapshot()
	oldMCP, ok := store.MCPConfig("attempt")
	require.True(t, ok)
	require.Equal(t, "http://attempt-0.example", oldMCP.URL)
	require.False(t, store.ConfigStaleness().Dirty)

	attempts := 0
	store.reloadAfterDiskRead = func() {
		attempts++
		writeReloadAttemptConfig(t, configPath, attempts)
		store.reloadPendingMu.Lock()
		store.reloadPending = true
		store.reloadPendingMu.Unlock()
	}
	err := store.autoReloadAfterWrite(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrConfigReloadUnstable)
	require.ErrorIs(t, err, errReloadDiskChanged)
	require.Contains(t, err.Error(), fmt.Sprintf("after %d attempts", reloadMaxAttempts))
	require.Equal(t, reloadMaxAttempts, attempts)
	require.Same(t, oldConfig, store.Config())
	require.Equal(t, oldGeneration, store.Generation())
	require.True(t, store.ConfigStaleness().Dirty)
	require.True(t, store.reloadMu.TryLock(), "bounded reload must release reloadMu")
	store.reloadMu.Unlock()
	store.reloadPendingMu.Lock()
	require.True(t, store.reloadPending, "exhaustion must preserve a queued writer")
	store.reloadPendingMu.Unlock()

	store.reloadAfterDiskRead = nil
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	store.reloadPendingMu.Lock()
	require.False(t, store.reloadPending, "the later stable reload must consume the queued writer")
	store.reloadPendingMu.Unlock()
	latest, ok := store.MCPConfig("attempt")
	require.True(t, ok)
	require.Equal(t, fmt.Sprintf("http://attempt-%d.example", reloadMaxAttempts), latest.URL)
	require.False(t, store.ConfigStaleness().Dirty)
}

func TestConfigStoreReloadExternalMCPInputsAreConsistent(t *testing.T) {
	t.Run("content change", func(t *testing.T) {
		store, root := isolatedMCPConfigStore(t)
		path := filepath.Join(root, ".mcp.json")
		writeExternalMCPServers(t, path, map[string]string{"changed.external": "http://old.example"})
		require.NoError(t, store.ReloadFromDisk(context.Background()))

		runExternalMutationDuringReload(t, store, func() {
			// Keep the replacement the same size so content hashing, rather than
			// timestamp or size luck, is what rejects the stale candidate.
			writeExternalMCPServers(t, path, map[string]string{"changed.external": "http://new.example"})
		})

		mcpConfig, ok := store.MCPConfig("changed.external")
		require.True(t, ok)
		require.Equal(t, "http://new.example", mcpConfig.URL)
		require.Equal(t, MCPSourceExternal, mcpConfig.Source)
		require.False(t, store.ConfigStaleness().Dirty)
	})

	t.Run("file added", func(t *testing.T) {
		store, root := isolatedMCPConfigStore(t)
		path := filepath.Join(root, ".mcp.json")
		require.NoError(t, store.ReloadFromDisk(context.Background()))

		runExternalMutationDuringReload(t, store, func() {
			writeExternalMCPServers(t, path, map[string]string{"added.external": "http://added.example"})
		})

		mcpConfig, ok := store.MCPConfig("added.external")
		require.True(t, ok)
		require.Equal(t, "http://added.example", mcpConfig.URL)
		require.Equal(t, MCPSourceExternal, mcpConfig.Source)
		require.False(t, store.ConfigStaleness().Dirty)
	})

	t.Run("file deleted", func(t *testing.T) {
		store, root := isolatedMCPConfigStore(t)
		path := filepath.Join(root, ".mcp.json")
		writeExternalMCPServers(t, path, map[string]string{"deleted.external": "http://deleted.example"})
		require.NoError(t, store.ReloadFromDisk(context.Background()))

		runExternalMutationDuringReload(t, store, func() {
			require.NoError(t, os.Remove(path))
		})

		_, ok := store.MCPConfig("deleted.external")
		require.False(t, ok)
		require.False(t, store.ConfigStaleness().Dirty)
	})
}

func runExternalMutationDuringReload(t *testing.T, store *ConfigStore, mutate func()) {
	t.Helper()
	read := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	store.reloadAfterExternalRead = func() {
		once.Do(func() {
			close(read)
			<-release
		})
	}

	done := make(chan error, 1)
	go func() {
		done <- store.ReloadFromDisk(context.Background())
	}()
	<-read
	mutate()
	close(release)
	require.NoError(t, <-done)
	store.reloadAfterExternalRead = nil
}

func writeReloadAttemptConfig(t *testing.T, path string, attempt int) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"mcp": map[string]any{
			"attempt": MCPConfig{
				Type: MCPHttp,
				URL:  fmt.Sprintf("http://attempt-%d.example", attempt),
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func writeExternalMCPServers(t *testing.T, path string, servers map[string]string) {
	t.Helper()
	entries := make(map[string]any, len(servers))
	for name, url := range servers {
		entries[name] = map[string]any{"type": "http", "url": url}
	}
	data, err := json.Marshal(map[string]any{"mcpServers": entries})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}
