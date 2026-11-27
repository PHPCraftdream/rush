package config

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReloadFromDisk_FinalFingerprintReadDoesNotBlockRuntimePublication(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"options":{"debug":false}}`), 0o600))
	isolateAllGlobalConfigPaths(t)
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	blockNextRead := atomic.Bool{}
	configTestHooks.Lock()
	previous := configTestHooks.beforeOpen
	var finalCheckOnce sync.Once
	configTestHooks.beforeOpen = func(path string) {
		if normalizeReloadPath(path) != normalizeReloadPath(configPath) {
			return
		}
		if blockNextRead.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeOpen = previous
		configTestHooks.Unlock()
	})
	*store.ReloadBeforeFinalInputCheckForTest() = func() {
		finalCheckOnce.Do(func() { blockNextRead.Store(true) })
	}

	reloadDone := make(chan error, 1)
	go func() { reloadDone <- store.ReloadFromDisk(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("reload did not reach its final fingerprint read")
	}

	publicationDone := make(chan struct{})
	go func() {
		store.SetProviderRuntimeConfig("runtime-only", ProviderConfig{ID: "runtime-only", APIKey: "fresh"})
		close(publicationDone)
	}()
	select {
	case <-publicationDone:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("runtime provider publication waited for the final fingerprint read")
	}
	close(release)
	select {
	case err := <-reloadDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("reload did not finish after the fingerprint read was released")
	}
}

func TestReloadFromDisk_DiskWriteAfterVerificationFencesCandidate(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"options":{"debug":false}}`), 0o600))
	isolateAllGlobalConfigPaths(t)
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	writeDone := make(chan struct{})
	*store.ReloadBeforePublishForTest() = func() {
		*store.ReloadBeforePublishForTest() = nil
		err := store.WithConfigWriteLock(configPath, func(target configWriteTarget) error {
			_, fingerprint, readErr := readStableConfigFileOwned(target.WTSelectedPath(), target.WTOwner(), target.WTEnforce())
			if readErr != nil {
				return readErr
			}
			_, commitErr := commitConfigFile(target.WTSelectedPath(), target.WTPath(), []byte(`{"options":{"debug":true}}`), 0o600, fingerprint, target.WTOwner(), target.WTEnforce())
			return commitErr
		})
		require.NoError(t, err)
		close(writeDone)
	}

	require.NoError(t, store.ReloadFromDisk(context.Background()))
	select {
	case <-writeDone:
	default:
		t.Fatal("test write did not run between final verification and publication")
	}
	require.True(t, store.Config().Options.Debug)
}
