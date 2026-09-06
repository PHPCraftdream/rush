//go:build !windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestRenameConfigTempAtPublishesNoReplaceWithOneDestinationLink(t *testing.T) {
	root := t.TempDir()
	tempName := ".rush.json.test.tmp"
	destinationName := "rush.json"
	require.NoError(t, os.WriteFile(filepath.Join(root, tempName), []byte(`{"new":true}`), 0o600))

	dirFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	dir := os.NewFile(uintptr(dirFD), root)
	t.Cleanup(func() { _ = dir.Close() })

	published, err := renameConfigTempAt(dirFD, tempName, dirFD, destinationName, false)
	require.NoError(t, err)
	require.True(t, published)
	_, err = os.Stat(filepath.Join(root, tempName))
	require.ErrorIs(t, err, os.ErrNotExist)

	destination, err := os.Open(filepath.Join(root, destinationName))
	require.NoError(t, err)
	info, err := destination.Stat()
	require.NoError(t, err)
	require.Equal(t, uint64(1), configFileNlinkOfOpened(destination, info))
	require.NoError(t, destination.Close())
}

func TestRenameConfigTempAtNoReplaceLeavesExistingDestination(t *testing.T) {
	root := t.TempDir()
	tempName := ".rush.json.test.tmp"
	destinationName := "rush.json"
	require.NoError(t, os.WriteFile(filepath.Join(root, tempName), []byte(`{"new":true}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, destinationName), []byte(`{"old":true}`), 0o600))

	dirFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	dir := os.NewFile(uintptr(dirFD), root)
	t.Cleanup(func() { _ = dir.Close() })

	published, err := renameConfigTempAt(dirFD, tempName, dirFD, destinationName, false)
	require.Error(t, err)
	require.False(t, published)
	require.Equal(t, []byte(`{"old":true}`), mustReadFile(t, filepath.Join(root, destinationName)))
	require.Equal(t, []byte(`{"new":true}`), mustReadFile(t, filepath.Join(root, tempName)))
}

func TestCommitLinkatUnlinkFailureUsesPostCommitReconciliation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	data := []byte(`{"new":true}`)
	_, expected, err := readStableConfigFile(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	unlinkErr := errors.New("injected temporary-name unlink failure")
	parentSyncErr := errors.New("injected parent fsync failure")
	var unlinkOnce sync.Once
	var parentSynced bool
	configTestHooks.Lock()
	previousForce := configTestHooks.forceLinkNoReplace
	previousUnlink := configTestHooks.unlinkTemp
	previousSync := configTestHooks.syncParent
	configTestHooks.forceLinkNoReplace = true
	configTestHooks.unlinkTemp = func(dirFD int, name string) error {
		failed := false
		unlinkOnce.Do(func() { failed = true })
		if failed {
			return unlinkErr
		}
		return unix.Unlinkat(dirFD, name, 0)
	}
	configTestHooks.syncParent = func(string) error {
		parentSynced = true
		return parentSyncErr
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.forceLinkNoReplace = previousForce
		configTestHooks.unlinkTemp = previousUnlink
		configTestHooks.syncParent = previousSync
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, data, 0o600, expected, 0, false)
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.True(t, outcome.Committed)
	require.True(t, outcome.Reconciled)
	require.ErrorIs(t, outcome, unlinkErr)
	require.ErrorIs(t, outcome, parentSyncErr)
	require.ErrorIs(t, outcome, errConfigCommitDurabilityUncertain)
	require.True(t, parentSynced)
	require.Equal(t, data, mustReadFile(t, path))
	entries, readDirErr := os.ReadDir(root)
	require.NoError(t, readDirErr)
	require.Len(t, entries, 1)
	require.Equal(t, "rush.json", entries[0].Name())
}
