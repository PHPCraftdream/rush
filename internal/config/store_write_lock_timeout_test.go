package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestRemoveConfigFieldBestEffort_BoundedByInternalTimeout proves the
// best-effort path selects its internal lock budget without waiting on it.
func TestRemoveConfigFieldBestEffort_BoundedByInternalTimeout(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	const key = "providers.anthropic.oauth"
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"anthropic":{"oauth":{"access_token":"secret"}}}}`), 0o600))

	store := newTestConfigStore(testStoreOpts{globalDataPath: configPath})
	var observedTimeout time.Duration
	var acquireCalls int
	configTestHooks.Lock()
	previousAcquire := configTestHooks.acquireConfigLock
	previousTimeout := configTestHooks.withConfigTimeout
	configTestHooks.withConfigTimeout = func(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
		observedTimeout = timeout
		ctx, cancel := context.WithTimeout(parent, timeout)
		cancel()
		return ctx, cancel
	}
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*session.FileLock, error) {
		acquireCalls++
		_, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline, "lock acquisition must receive a deadline")
		require.ErrorIs(t, ctx.Err(), context.Canceled, "lock acquisition must receive a canceled context")
		require.Equal(t, normalizeReloadPath(configPath)+".lock", path)
		return nil, context.DeadlineExceeded
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.acquireConfigLock = previousAcquire
		configTestHooks.withConfigTimeout = previousTimeout
		configTestHooks.Unlock()
	})

	store.removeConfigFieldBestEffort(ScopeGlobal, key)
	require.Equal(t, internalConfigWriteLockTimeout, observedTimeout,
		"best-effort removal must inject the internal lock timeout")
	require.Equal(t, 1, acquireCalls, "best-effort removal must make one controlled acquisition")

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.True(t, gjson.Get(string(data), key).Exists(),
		"key must still be present after controlled lock acquisition failure")
}

// TestRemoveConfigFieldBestEffort_PreservesContentWhenExternalLockHeld keeps
// the real sidecar-lock content-preservation check independent of the timeout
// oracle above.
func TestRemoveConfigFieldBestEffort_PreservesContentWhenExternalLockHeld(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	const key = "providers.anthropic.oauth"
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"anthropic":{"oauth":{"access_token":"secret"}}}}`), 0o600))

	externalLock, err := session.TryAcquireFileLock(configPath + ".lock")
	require.NoError(t, err, "test setup: must be able to take the sidecar lock before the call under test runs")
	t.Cleanup(func() { _ = externalLock.Release() })

	configTestHooks.Lock()
	previousTimeout := configTestHooks.withConfigTimeout
	configTestHooks.withConfigTimeout = func(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithTimeout(parent, timeout)
		cancel()
		return ctx, cancel
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.withConfigTimeout = previousTimeout
		configTestHooks.Unlock()
	})

	store := newTestConfigStore(testStoreOpts{globalDataPath: configPath})
	store.removeConfigFieldBestEffort(ScopeGlobal, key)

	data, rerr := os.ReadFile(configPath)
	require.NoError(t, rerr)
	assert.True(t, gjson.Get(string(data), key).Exists(),
		"key must still be present when the external sidecar lock is held")
}

// TestRemoveConfigFieldBestEffort_SucceedsQuicklyWhenLockFree is the control
// case: a free lock is acquired, released, and the key is removed.
func TestRemoveConfigFieldBestEffort_SucceedsQuicklyWhenLockFree(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	const key = "providers.anthropic.oauth"
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"anthropic":{"oauth":{"access_token":"secret"}}}}`), 0o600))

	store := newTestConfigStore(testStoreOpts{globalDataPath: configPath})
	entered := make(chan string, 1)
	acquired := make(chan string, 1)
	released := make(chan string, 1)
	attempts := 0
	configTestHooks.Lock()
	previousAcquire := configTestHooks.acquireConfigLock
	previousRelease := configTestHooks.releaseConfigLock
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*session.FileLock, error) {
		attempts++
		entered <- path
		lock, err := session.TryAcquireFileLock(path)
		if err == nil {
			acquired <- path
		}
		return lock, err
	}
	configTestHooks.releaseConfigLock = func(path string, lock *session.FileLock) error {
		err := lock.Release()
		if err == nil {
			released <- path
		}
		return err
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.acquireConfigLock = previousAcquire
		configTestHooks.releaseConfigLock = previousRelease
		configTestHooks.Unlock()
	})

	store.removeConfigFieldBestEffort(ScopeGlobal, key)
	require.Equal(t, 1, attempts, "lock-free removal must make one acquisition call")
	require.Equal(t, configPath+".lock", <-entered)
	require.Equal(t, configPath+".lock", <-acquired)
	require.Equal(t, configPath+".lock", <-released)

	data, rerr := os.ReadFile(configPath)
	require.NoError(t, rerr)
	assert.False(t, gjson.Get(string(data), key).Exists(),
		"key must have been removed from disk when the lock was immediately available")
}
