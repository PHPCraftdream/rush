package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/stretchr/testify/require"
)

func TestCycle7LoadFinalVerifyRejectsProviderPhaseWrite(t *testing.T) {
	tests := []struct {
		name    string
		initial string
	}{
		{
			name:    "unconfigured publish",
			initial: `{"options":{"debug":false}}`,
		},
		{
			name: "configured publish",
			initial: `{
				"options":{"debug":false,"disable_provider_auto_update":true},
				"providers":{"custom":{"base_url":"https://example.invalid/v1","api_key":"test-key","models":[{"id":"model","name":"Model"}]}},
				"models":{"smart":{"provider":"custom","model":"model"},"fast":{"provider":"custom","model":"model"}}
			}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateAllGlobalConfigPaths(t)
			root := t.TempDir()
			path := filepath.Join(root, "rush.json")
			updated := strings.Replace(tt.initial, `"debug":false`, `"debug":true`, 1)
			require.NotEqual(t, tt.initial, updated)
			require.NoError(t, os.WriteFile(path, []byte(tt.initial), 0o600))

			var mutated atomic.Bool
			setLoadAfterProviderConfigHook(t, func(workingDir string) {
				if normalizeReloadPath(workingDir) != normalizeReloadPath(root) || !mutated.CompareAndSwap(false, true) {
					return
				}
				require.NoError(t, os.WriteFile(path, []byte(updated), 0o600))
			})

			store, err := Load(root, filepath.Join(root, "data"), false)
			require.NoError(t, err)
			require.True(t, mutated.Load())
			require.True(t, store.Config().Options.Debug)
			var disk Config
			require.NoError(t, json.Unmarshal(mustReadFile(t, path), &disk))
			require.NotNil(t, disk.Options)
			require.True(t, disk.Options.Debug)
			require.False(t, store.ConfigStaleness().Dirty, "retried load must snapshot the bytes it finally published")
		})
	}
}

func TestCycle7DisableNonWritableOriginsFailsClosed(t *testing.T) {
	t.Run("project", func(t *testing.T) {
		isolateAllGlobalConfigPaths(t)
		root := t.TempDir()
		globalPath := filepath.Join(root, "global-data", "rush.json")
		workspacePath := filepath.Join(root, "workspace-data", "rush.json")
		setMCPFile(t, filepath.Join(root, "rush.json"), "server", "http://project.example")
		store := newTestConfigStore(testStoreOpts{
			config:         &Config{},
			globalDataPath: globalPath,
			workspacePath:  workspacePath,
		})
		store.workingDir = root

		_, err := store.PersistMCPDisabledOverrideResult(ScopeGlobal, "server", true)
		require.ErrorIs(t, err, ErrMCPUnwritableOrigin)
		requireNoConfigFile(t, globalPath)
		requireNoConfigFile(t, workspacePath)
	})

	t.Run("system", func(t *testing.T) {
		isolateAllGlobalConfigPaths(t)
		root := t.TempDir()
		globalPath := filepath.Join(root, "global-data", "rush.json")
		workspacePath := filepath.Join(root, "workspace-data", "rush.json")
		systemPath := filepath.Join(root, "system", "rush.json")
		setMCPFile(t, systemPath, "server", "http://system.example")
		systemOwner, ownerErr := fsext.Owner(systemPath)
		require.NoError(t, ownerErr)
		store := newTestConfigStore(testStoreOpts{
			config:         &Config{},
			globalDataPath: globalPath,
			workspacePath:  workspacePath,
		})
		store.workingDir = root
		store.systemConfigPathOverride = systemPath

		_, err := store.PersistMCPDisabledOverrideResult(ScopeWorkspace, "server", true)
		if systemOwner == systemConfigOwner() {
			require.ErrorIs(t, err, ErrMCPUnwritableOrigin)
		} else {
			require.ErrorIs(t, err, ErrMCPNotFound)
		}
		requireNoConfigFile(t, globalPath)
		requireNoConfigFile(t, workspacePath)
	})
}

func setLoadAfterProviderConfigHook(t *testing.T, hook func(string)) {
	t.Helper()
	loadAfterProviderConfigTestHook.Lock()
	previous := loadAfterProviderConfigTestHook.fn
	loadAfterProviderConfigTestHook.fn = hook
	loadAfterProviderConfigTestHook.Unlock()
	t.Cleanup(func() {
		loadAfterProviderConfigTestHook.Lock()
		loadAfterProviderConfigTestHook.fn = previous
		loadAfterProviderConfigTestHook.Unlock()
	})
}

func requireNoConfigFile(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}
