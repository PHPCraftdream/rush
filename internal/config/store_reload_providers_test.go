// ReloadFromDisk must reject a candidate config that would silently drop
// a live custom provider because its base_url is missing or empty (see
// store_reload_providers.go). Covers: live provider dropped → error and
// untouched snapshot; never-live provider dropped → reload still works;
// provider removed from disk entirely → reload still works.

package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func reloadEndpointTestStore(t *testing.T, configJSON string) (*ConfigStore, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	require.NoError(t, os.WriteFile(configPath, []byte(configJSON), 0o600))

	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})
	return store, configPath
}

// TestReloadFromDisk_RejectsEndpointDropOfLiveProvider pins task #1467:
// removing base_url from a live enabled custom provider must fail the
// reload with the provider id and "base_url" in the error, and the
// previous snapshot (providers, models, generation) must stay untouched.
func TestReloadFromDisk_RejectsEndpointDropOfLiveProvider(t *testing.T) {
	store, configPath := reloadEndpointTestStore(t, `{
		"models": {
			"smart": {"provider": "myprov", "model": "m1"}
		},
		"providers": {
			"myprov": {
				"base_url": "https://api.myprov.example/v1",
				"api_key": "test-key",
				"models": [{"id": "m1", "name": "M1"}]
			}
		}
	}`)

	beforeCfg, beforeGen := store.Snapshot()
	_, ok := beforeCfg.Providers.Get("myprov")
	require.True(t, ok, "provider must be live after the initial Load")

	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"models": {
			"smart": {"provider": "myprov", "model": "m1"}
		},
		"providers": {
			"myprov": {
				"api_key": "test-key",
				"models": [{"id": "m1", "name": "M1"}]
			}
		}
	}`), 0o600))

	err := store.ReloadFromDisk(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, errReloadProviderEndpointDropped)
	require.ErrorContains(t, err, "myprov")
	require.ErrorContains(t, err, "base_url")

	afterCfg, afterGen := store.Snapshot()
	require.Equal(t, beforeGen, afterGen, "generation must be untouched after a rejected reload")
	_, ok = afterCfg.Providers.Get("myprov")
	require.True(t, ok, "live provider must survive a rejected reload")
	require.Equal(t, "myprov", afterCfg.Models[SelectedModelTypeSmart].Provider, "role pin must still resolve after a rejected reload")
}

// TestReloadFromDisk_NeverLiveProviderEndpointDropDoesNotBlock pins that a
// provider configureProviders already dropped on the initial Load (never
// live) does not make a later unrelated reload fail.
func TestReloadFromDisk_NeverLiveProviderEndpointDropDoesNotBlock(t *testing.T) {
	store, configPath := reloadEndpointTestStore(t, `{
		"providers": {
			"broken": {
				"api_key": "test-key",
				"models": [{"id": "m1", "name": "M1"}]
			},
			"good": {
				"base_url": "https://api.good.example/v1",
				"api_key": "test-key",
				"models": [{"id": "m1", "name": "M1"}]
			}
		}
	}`)

	_, ok := store.Config().Providers.Get("broken")
	require.False(t, ok, "provider without base_url must not be live after the initial Load")

	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"options": {"debug": true},
		"providers": {
			"broken": {
				"api_key": "test-key",
				"models": [{"id": "m1", "name": "M1"}]
			},
			"good": {
				"base_url": "https://api.good.example/v1",
				"api_key": "test-key",
				"models": [{"id": "m1", "name": "M1"}]
			}
		}
	}`), 0o600))

	require.NoError(t, store.ReloadFromDisk(context.Background()))
}

// TestReloadFromDisk_RemovingProviderFromDiskStillReloads pins that
// removing a live custom provider entirely from disk (not an endpoint
// validation drop) still reloads fine and the provider disappears.
func TestReloadFromDisk_RemovingProviderFromDiskStillReloads(t *testing.T) {
	store, configPath := reloadEndpointTestStore(t, `{
		"providers": {
			"myprov": {
				"base_url": "https://api.myprov.example/v1",
				"api_key": "test-key",
				"models": [{"id": "m1", "name": "M1"}]
			}
		}
	}`)

	_, ok := store.Config().Providers.Get("myprov")
	require.True(t, ok)

	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"options": {"debug": true}
	}`), 0o600))

	require.NoError(t, store.ReloadFromDisk(context.Background()))
	_, ok = store.Config().Providers.Get("myprov")
	require.False(t, ok, "provider removed from disk must disappear after reload")
}
