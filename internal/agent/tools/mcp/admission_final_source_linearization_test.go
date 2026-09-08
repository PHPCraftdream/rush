package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestMCPAdmissionFinalSourceLinearizationRejectsInPlaceChanges(t *testing.T) {
	t.Run("project and external sources", func(t *testing.T) {
		store := isolatedMCPStore(t)
		const name = "final-source-linearization"
		projectPath := filepath.Join(store.WorkingDir(), "rush.json")
		externalPath := filepath.Join(store.WorkingDir(), ".mcp.json")
		writeMCPAdmissionSource(t, projectPath, "mcp", name, "http://project-old.example")
		writeMCPAdmissionSource(t, externalPath, "mcpServers", name, "http://external-old.example")
		require.NoError(t, store.ReloadFromDisk(context.Background()))
		runMCPAdmissionWhileLifecycleBlocked(t, store, name, func() {
			overwriteMCPAdmissionSource(t, projectPath, "mcp", name, "http://project-new.example")
			overwriteMCPAdmissionSource(t, externalPath, "mcpServers", name, "http://external-new.example")
		}, true)
	})

	t.Run("unchanged source succeeds", func(t *testing.T) {
		store := isolatedMCPStore(t)
		const name = "final-source-unchanged"
		path := filepath.Join(store.WorkingDir(), "rush.json")
		writeMCPAdmissionSource(t, path, "mcp", name, "http://unchanged.example")
		require.NoError(t, store.ReloadFromDisk(context.Background()))
		runMCPAdmissionWhileLifecycleBlocked(t, store, name, func() {}, false)
	})
}

func TestMCPAdmissionFinalSourceLinearizationRejectsExpectedAbsentCreation(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "final-source-absent"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type: config.MCPHttp, URL: "http://global.example",
	}))
	projectPath := filepath.Join(store.WorkingDir(), "rush.json")
	if err := os.Remove(projectPath); err != nil {
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	runMCPAdmissionWhileLifecycleBlocked(t, store, name, func() {
		writeMCPAdmissionSource(t, projectPath, "mcp", name, "http://project-created.example")
	}, true)
}

func runMCPAdmissionWhileLifecycleBlocked(t *testing.T, store *config.ConfigStore, name string, change func(), wantStale bool) {
	t.Helper()
	snapshot := store.SnapshotMCPAdmission(name)
	finalRevalidated := make(chan struct{})
	mcpInitTestHooks.Lock()
	previous := mcpInitTestHooks.afterAdmissionFinalRevalidate
	mcpInitTestHooks.afterAdmissionFinalRevalidate = func(hookName string) {
		if hookName == name {
			close(finalRevalidated)
		}
	}
	mcpInitTestHooks.Unlock()
	t.Cleanup(func() {
		mcpInitTestHooks.Lock()
		mcpInitTestHooks.afterAdmissionFinalRevalidate = previous
		mcpInitTestHooks.Unlock()
	})

	var published atomic.Bool
	done := make(chan error, 1)
	lifecycleMu.Lock()
	released := false
	defer func() {
		if !released {
			lifecycleMu.Unlock()
		}
	}()
	go func() {
		done <- store.WithCurrentMCPAdmission(snapshot, name, func(guard config.MCPAdmissionGuard) error {
			return withMCPAdmissionFinalTurn(context.Background(), name, guard, func() error {
				published.Store(true)
				return nil
			})
		})
	}()
	awaitMCPSignal(t, finalRevalidated)
	change()
	lifecycleMu.Unlock()
	released = true

	err := <-done
	if wantStale {
		require.ErrorIs(t, err, config.ErrMCPMutationStale)
		require.False(t, published.Load())
		return
	}
	require.NoError(t, err)
	require.True(t, published.Load())
}

func writeMCPAdmissionSource(t *testing.T, path, key, name, url string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	data, err := json.Marshal(map[string]any{
		key: map[string]any{name: map[string]any{
			"type": "http",
			"url":  url,
		}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func overwriteMCPAdmissionSource(t *testing.T, path, key, name, url string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		key: map[string]any{name: map[string]any{
			"type": "http",
			"url":  url,
		}},
	})
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Len(t, data, int(info.Size()), fmt.Sprintf("same-size mutation for %s", path))
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, writeErr := file.WriteAt(data, 0)
	syncErr := file.Sync()
	closeErr := file.Close()
	require.NoError(t, writeErr)
	require.NoError(t, syncErr)
	require.NoError(t, closeErr)
	require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
}
