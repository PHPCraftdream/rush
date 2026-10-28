package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestSetConfigFieldsRevalidatesSlotAfterWaitingForMigrationLock reproduces a
// writer choosing rush.json, pausing before its sidecar lock, and resuming
// after another process publishes rush.ktav as the active slot.
func TestSetConfigFieldsRevalidatesSlotAfterWaitingForMigrationLock(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "rush.json")
	legacyBytes := []byte(`{"options":{"debug":false}}`)
	require.NoError(t, os.WriteFile(legacy, legacyBytes, 0o600))
	store := newTestConfigStore(testStoreOpts{globalDataPath: legacy})

	entered := make(chan struct{})
	release := make(chan struct{})
	var pauseOnce sync.Once
	configTestHooks.Lock()
	previousAcquire := configTestHooks.acquireConfigLock
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*filelock.FileLock, error) {
		paused := false
		pauseOnce.Do(func() {
			paused = true
			close(entered)
		})
		if paused {
			<-release
		}
		return filelock.AcquireFileLockContext(ctx, path)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.acquireConfigLock = previousAcquire
		configTestHooks.Unlock()
		select {
		case <-release:
		default:
			close(release)
		}
	})

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- store.SetConfigFields(ScopeGlobal, map[string]any{"options.debug": true})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not reach the pre-lock pause")
	}

	require.NoError(t, runKtavMigrationState(context.Background(), legacy, dir))
	close(release)
	select {
	case err := <-writeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not finish after release")
	}

	active, err := os.ReadFile(ktavSiblingOf(legacy))
	require.NoError(t, err)
	decoded, err := ktavcodec.DecodeKtav(active)
	require.NoError(t, err, "active slot must remain valid Ktav")
	activeJSON, err := json.Marshal(decoded)
	require.NoError(t, err)
	require.True(t, gjson.Get(string(activeJSON), "options.debug").Bool(),
		"successful write must land in the now-active Ktav generation")
	require.Equal(t, legacyBytes, mustReadConfigSlotRegressionFile(t, legacy), "migration must preserve legacy JSON")
}

func mustReadConfigSlotRegressionFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}
