// Reload tests: ReloadFromDisk picking up new on-disk values, auto-reload
// suppression during reload, and provider updates racing ReloadFromDisk.

package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestReloadFromDisk_UsesNewConfigValues is a regression test ensuring that
// ReloadFromDisk updates store state BEFORE running model/agent setup,
// so the new config values are used rather than stale pre-reload values.
func TestReloadFromDisk_UsesNewConfigValues(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Isolate from the host's global config so only test-provided
	// providers are visible.
	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)

	// Create initial config with one model preference
	initialConfig := `{
		"models": {
			"large": {"provider": "openai", "model": "gpt-4"}
		},
		"providers": {
			"openai": {
				"api_key": "test-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config properly
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Set globalDataPath for the test (Load doesn't set this directly)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Verify initial model
	require.Equal(t, "openai", store.Config().Models[SelectedModelTypeLarge].Provider)
	require.Equal(t, "gpt-4", store.Config().Models[SelectedModelTypeLarge].Model)

	// Modify config on disk to change model
	updatedConfig := `{
		"models": {
			"large": {"provider": "anthropic", "model": "claude-3"}
		},
		"providers": {
			"openai": {
				"api_key": "test-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			},
			"anthropic": {
				"api_key": "test-key-2",
				"models": [{"id": "claude-3", "name": "Claude 3"}]
			}
		}
	}`
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(updatedConfig), 0o600))

	// Reload from disk
	ctx := context.Background()
	err = store.ReloadFromDisk(ctx)
	require.NoError(t, err)

	// Verify the NEW config values are now in effect (regression check)
	require.Equal(t, "anthropic", store.Config().Models[SelectedModelTypeLarge].Provider)
	require.Equal(t, "claude-3", store.Config().Models[SelectedModelTypeLarge].Model)
}

// TestAutoReloadDisabledDuringReload verifies that auto-reload is suppressed
// during ReloadFromDisk to prevent re-entrant/nested reload calls.
func TestAutoReloadDisabledDuringReload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create initial config with a provider that will trigger config modification during reload
	// (simulating the anthropic OAuth token removal case)
	initialConfig := `{
		"providers": {
			"anthropic": {
				"api_key": "test-key",
				"oauth": {"access_token": "token", "refresh_token": "refresh"}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load will trigger configureProviders which removes anthropic OAuth config.
	// This should NOT cause infinite recursion thanks to the publishMu guard
	// (the re-entrant auto-reload is skipped via TryLock).
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Verify the store loaded successfully and publishMu was released.
	require.True(t, store.publishMu.TryLock(), "publishMu should be free after Load")
	store.publishMu.Unlock()

	// Capture snapshot and verify reload also works without recursion
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Modify file and reload - this should work without re-entrancy issues
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"options": {"debug": true}}`), 0o600))

	err = store.ReloadFromDisk(context.Background())
	require.NoError(t, err)

	// Verify reload completed successfully and publishMu was released.
	require.True(t, store.publishMu.TryLock(), "publishMu should be free after ReloadFromDisk")
	store.publishMu.Unlock()
}

// TestProviderUpdates_ConcurrentReloadNoRace runs SetProviderRuntimeConfig
// and ReloadFromDisk concurrently to verify (via the -race detector) that
// the publishMu guard prevents any data race between the in-memory provider
// update and the reload's full snapshot rebuild.
func TestProviderUpdates_ConcurrentReloadNoRace(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	initialConfig := `{
		"providers": {
			"openai": {
				"api_key": "disk-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	ctx := context.Background()
	var wg sync.WaitGroup
	var stop atomic.Bool

	// Reloader: continuously reloads from disk until stop is set.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = store.ReloadFromDisk(ctx)
		}
	}()

	// Writer: repeatedly applies runtime-only provider updates.
	for i := range 200 {
		pc, ok := store.Config().Providers.Get("openai")
		if !ok {
			continue
		}
		pc.APIKey = fmt.Sprintf("refreshed-key-%d", i)
		store.SetProviderRuntimeConfig("openai", pc)
	}

	stop.Store(true)
	wg.Wait()

	// After all concurrent activity, the store must still be consistent:
	// the provider is present (from the last reload or the last write).
	_, ok := store.Config().Providers.Get("openai")
	require.True(t, ok, "provider must still be present after concurrent updates")
}
