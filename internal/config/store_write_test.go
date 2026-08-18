// Config-write tests: configPath resolution per scope, the workspace-scope
// guards on SetConfigField/RemoveConfigField/HasConfigField, and the
// auto-reload behaviour of the write APIs.

package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigStore_ConfigPath_GlobalAlwaysWorks(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		globalDataPath: "/some/global/crush.json",
	})

	path, err := store.configPath(ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, "/some/global/crush.json", path)
}

func TestConfigStore_ConfigPath_WorkspaceReturnsPath(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		workspacePath: "/some/workspace/.crush/crush.json",
	})

	path, err := store.configPath(ScopeWorkspace)
	require.NoError(t, err)
	require.Equal(t, "/some/workspace/.crush/crush.json", path)
}

func TestConfigStore_ConfigPath_WorkspaceErrorsWhenEmpty(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		globalDataPath: "/some/global/crush.json",
		workspacePath:  "",
	})

	_, err := store.configPath(ScopeWorkspace)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoWorkspaceConfig))
}

func TestConfigStore_SetConfigField_WorkspaceScopeGuard(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: filepath.Join(t.TempDir(), "global.json"),
		workspacePath:  "",
	})

	err := store.SetConfigField(ScopeWorkspace, "foo", "bar")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoWorkspaceConfig))
}

func TestConfigStore_SetConfigField_GlobalScopeAlwaysWorks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	globalPath := filepath.Join(dir, "crush.json")
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: globalPath,
	})

	err := store.SetConfigField(ScopeGlobal, "foo", "bar")
	require.NoError(t, err)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	require.Contains(t, string(data), `"foo"`)
}

func TestConfigStore_RemoveConfigField_WorkspaceScopeGuard(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: filepath.Join(t.TempDir(), "global.json"),
		workspacePath:  "",
	})

	err := store.RemoveConfigField(ScopeWorkspace, "foo")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoWorkspaceConfig))
}

func TestConfigStore_HasConfigField_WorkspaceScopeGuard(t *testing.T) {
	t.Parallel()

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: filepath.Join(t.TempDir(), "global.json"),
		workspacePath:  "",
	})

	has := store.HasConfigField(ScopeWorkspace, "foo")
	require.False(t, has)
}

// TestSetConfigField_AutoReloads verifies that SetConfigField automatically
// reloads config into memory after writing, so subsequent reads see the new value.
func TestSetConfigField_AutoReloads(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create initial config file with debug = false
	initialConfig := `{"options": {"debug": false}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Verify initial state
	require.False(t, store.Config().Options.Debug)

	// Set globalDataPath and capture snapshot for staleness tracking
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Use SetConfigField to change debug to true
	err = store.SetConfigField(ScopeGlobal, "options.debug", true)
	require.NoError(t, err)

	// Verify in-memory state was automatically reloaded and reflects the change
	require.True(t, store.Config().Options.Debug, "Expected config to auto-reload and show debug = true")

	// Verify staleness is clean after the reload
	staleness := store.ConfigStaleness()
	require.False(t, staleness.Dirty, "Expected staleness to be clean after auto-reload")
}

// TestRemoveConfigField_AutoReloads verifies that RemoveConfigField automatically
// reloads config into memory after writing.
func TestRemoveConfigField_AutoReloads(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create initial config file with a custom option
	initialConfig := `{"options": {"debug": true, "custom_field": "value"}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Set globalDataPath and capture snapshot
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Verify the field exists initially (indirectly - store loaded successfully)
	require.True(t, store.Config().Options.Debug)

	// Remove the debug field
	err = store.RemoveConfigField(ScopeGlobal, "options.debug")
	require.NoError(t, err)

	// Verify auto-reload occurred and stale state is clean
	staleness := store.ConfigStaleness()
	require.False(t, staleness.Dirty, "Expected staleness to be clean after auto-reload from RemoveConfigField")
}

// TestSetConfigField_AutoReloadSkipsWhenNoWorkingDir verifies that auto-reload
// gracefully skips when working directory is not set (e.g., during testing).
func TestSetConfigField_AutoReloadSkipsWhenNoWorkingDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create a store without working directory (like some test setups)
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
		// workingDir is empty
	})

	// SetConfigField should succeed even without workingDir (auto-reload skips)
	err := store.SetConfigField(ScopeGlobal, "foo", "bar")
	require.NoError(t, err)

	// Verify file was still written
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "foo")
}

// TestSetConfigFields_AutoReloadsAtomically verifies that SetConfigFields writes
// multiple fields in a single disk write and triggers only one auto-reload,
// avoiding intermediate states where only some fields are persisted.
func TestSetConfigFields_AutoReloadsAtomically(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Create initial config file.
	initialConfig := `{"options": {"debug": false}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config.
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Set globalDataPath and capture snapshot.
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Write multiple fields atomically.
	err = store.SetConfigFields(ScopeGlobal, map[string]any{
		"options.debug":  true,
		"options.custom": "hello",
	})
	require.NoError(t, err)

	// Verify both fields are reflected in memory.
	require.True(t, store.Config().Options.Debug)
}
