package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAtomicWriteParentSyncFailureIsTypedCommittedError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o600))
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

	err := atomicWriteFile(path, []byte(`{"new":true}`), 0o600)
	require.ErrorIs(t, err, errAtomicWriteCommitted)
	require.Equal(t, []byte(`{"new":true}`), mustReadFile(t, path))
}

func TestSetConfigFieldsParentSyncFailureReconcilesAndReturnsUncertainty(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"options":{"debug":false}}`), 0o600))
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{Options: &Options{}},
		globalDataPath: path,
	})
	store.workingDir = root
	store.captureStalenessSnapshot([]string{path})
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

	err := store.SetConfigField(ScopeGlobal, "options.debug", true)
	require.ErrorIs(t, err, errAtomicWriteCommitted)
	require.True(t, store.Config().Options.Debug)
	require.False(t, store.ConfigStaleness().Dirty)
	_, fingerprint, readErr := readStableConfigFile(path)
	require.NoError(t, readErr)
	require.Equal(t, fingerprint, store.loadSnapshot().snapshots[normalizeDiscoveryPath(path)].fingerprint)
}

func TestRemoveConfigFieldParentSyncFailureReconcilesAndReturnsUncertainty(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"obsolete":true}`), 0o600))
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{Options: &Options{}},
		globalDataPath: path,
	})
	store.workingDir = root
	store.captureStalenessSnapshot([]string{path})
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

	err := store.RemoveConfigField(ScopeGlobal, "obsolete")
	require.ErrorIs(t, err, errAtomicWriteCommitted)
	require.NotContains(t, string(mustReadFile(t, path)), "obsolete")
	require.False(t, store.ConfigStaleness().Dirty)
	_, fingerprint, readErr := readStableConfigFile(path)
	require.NoError(t, readErr)
	require.Equal(t, fingerprint, store.loadSnapshot().snapshots[normalizeDiscoveryPath(path)].fingerprint)
}
