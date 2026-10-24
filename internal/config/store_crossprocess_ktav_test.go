// Cross-process (inter-process sidecar lock) Ktav variants: two ConfigStores
// over ONE rush.ktav file must not lose updates, the deterministic lock-free
// negative control must still lose one, and the lock-timeout behavior of the
// write path must hold against a .ktav target. The sidecar lock is anchored
// on the LEGACY JSON path: rush.json.lock even for a rush.ktav store, so
// Ktav and pre-Ktav processes contend on the same sidecar.
package config

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TestSetConfigFieldsTwoStoresSameKtavFileBothUpdatesSurvive is the Ktav
// variant of TestSetConfigFields_TwoStoresSameFile_BothUpdatesSurvive: two
// ConfigStores (two private diskWriteMus, i.e. two rush processes) write
// DIFFERENT keys to the SAME rush.ktav; only the inter-process sidecar lock
// (the legacy-anchored rush.json.lock) prevents the lost update. Both updates must survive, and
// the file must remain a valid Ktav document.
func TestSetConfigFieldsTwoStoresSameKtavFileBothUpdatesSurvive(t *testing.T) {
	// Keep this global file-publication test out of package parallelism.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.ktav")
	var releaseFirstCommit sync.Once
	firstCommit := make(chan struct{})
	secondAcquireReady := make(chan struct{})
	allowSecondProbe := make(chan struct{})
	releaseFirst := make(chan struct{})
	var allowSecondProbeOnce sync.Once
	secondProbeResult := make(chan error, 1)
	acquireEntered := make(chan int, 2)
	acquireAcquired := make(chan int, 2)
	var acquireCalls atomic.Int32
	var commitCalls atomic.Int32
	configTestHooks.Lock()
	previousBeforeCommitRename := configTestHooks.beforeCommitRename
	previousAcquire := configTestHooks.acquireConfigLock
	configTestHooks.beforeCommitRename = func() {
		if commitCalls.Add(1) == 1 {
			close(firstCommit)
			<-releaseFirst
		}
	}
	var acquireOrder sync.Mutex
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*filelock.FileLock, error) {
		acquireOrder.Lock()
		id := int(acquireCalls.Add(1))
		acquireEntered <- id
		acquireOrder.Unlock()
		if id == 2 {
			close(secondAcquireReady)
			<-allowSecondProbe
			probe, probeErr := filelock.TryAcquireFileLock(path)
			secondProbeResult <- probeErr
			if probeErr == nil {
				acquireAcquired <- id
				return probe, nil
			}
			<-releaseFirst
			lock, err := filelock.AcquireFileLockContext(ctx, path)
			if err == nil {
				acquireAcquired <- id
			}
			return lock, err
		}
		lock, err := filelock.AcquireFileLockContext(ctx, path)
		if err == nil {
			acquireAcquired <- id
		}
		return lock, err
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeCommitRename = previousBeforeCommitRename
		configTestHooks.acquireConfigLock = previousAcquire
		configTestHooks.Unlock()
		releaseFirstCommit.Do(func() { close(releaseFirst) })
		allowSecondProbeOnce.Do(func() { close(allowSecondProbe) })
	})

	require.NoError(t, os.WriteFile(configPath, testKtavBytes(t, `{}`), 0o600))

	// Two independent stores sharing the SAME ktav globalDataPath — two
	// separate in-process mutexes, like two rush processes sharing one
	// config file. workingDir is "" so autoReload is a no-op.
	store1 := newTestConfigStore(testStoreOpts{globalDataPath: configPath})
	store2 := newTestConfigStore(testStoreOpts{globalDataPath: configPath})

	var wg sync.WaitGroup
	start := make(chan struct{})
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		err1 = store1.SetConfigFields(ScopeGlobal, map[string]any{
			"options.tui.theme": "dark",
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		err2 = store2.SetConfigFields(ScopeGlobal, map[string]any{
			"options.tui.compact_mode": true,
		})
	}()
	close(start)
	require.Equal(t, 1, <-acquireEntered)
	require.Equal(t, 2, <-acquireEntered)
	require.Equal(t, 1, <-acquireAcquired)
	require.Empty(t, acquireAcquired, "the second writer must not acquire before the first commit is released")
	<-firstCommit
	<-secondAcquireReady

	// The sidecar lock is anchored on the LEGACY JSON path: rush.json.lock,
	// even though the file being written is rush.ktav.
	sidecar := configWriteLockAnchor(configPath) + ".lock"
	probe, probeErr := filelock.TryAcquireFileLock(sidecar)
	if probeErr == nil {
		_ = probe.Release()
		require.FailNow(t, "the first writer must hold the sidecar lock while its commit is paused")
	}
	var contended *filelock.ErrLockContended
	require.ErrorAs(t, probeErr, &contended)
	require.FileExists(t, sidecar)
	require.NoFileExists(t, configPath+".lock",
		"the sidecar must be anchored on the legacy name, not the ktav target")

	allowSecondProbeOnce.Do(func() { close(allowSecondProbe) })
	secondProbeErr := <-secondProbeResult
	require.ErrorAs(t, secondProbeErr, &contended)
	releaseFirstCommit.Do(func() { close(releaseFirst) })
	require.Equal(t, 2, <-acquireAcquired)
	wg.Wait()

	require.NoError(t, err1)
	require.NoError(t, err2)

	doc := string(mustDecodeTestKtav(t, fileBytes(t, configPath)))
	require.True(t, gjson.Get(doc, "options.tui.theme").Exists(),
		"store1's key (theme) was lost — cross-process lost update on rush.ktav")
	require.True(t, gjson.Get(doc, "options.tui.compact_mode").Exists(),
		"store2's key (compact_mode) was lost — cross-process lost update on rush.ktav")
}

// TestUnprotectedKtavRMWDeterministicallyLosesUpdate is the Ktav variant of
// TestUnprotectedRMW_DeterministicallyLosesUpdate: the negative control
// proving the lost update the sidecar lock prevents is real for rush.ktav
// too. Two lock-free writers both read the same pre-write bytes, mutate their
// own key in the decoded JSON, re-encode, and overwrite; exactly one update
// survives.
func TestUnprotectedKtavRMWDeterministicallyLosesUpdate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.ktav")
	require.NoError(t, os.WriteFile(configPath, testKtavBytes(t, `{}`), 0o600))

	// Phase 1: both read the file BEFORE any write (barrier). Both see "{}".
	var readA, readB []byte
	var readWg sync.WaitGroup
	readWg.Add(2)
	for _, dst := range []*[]byte{&readA, &readB} {
		dst := dst
		go func() {
			defer readWg.Done()
			raw, err := os.ReadFile(configPath)
			if err != nil {
				raw = testKtavBytes(t, `{}`)
			}
			*dst = raw
		}()
	}
	readWg.Wait()

	// Phase 2: both write from their own (stale, identical) read. Errors go
	// through a channel: testing.T FailNow is only safe on the test
	// goroutine (same discipline as the JSON control).
	errCh := make(chan error, 2)
	var writeWg sync.WaitGroup
	writeWg.Add(2)
	mutations := []struct {
		key   string
		value any
	}{
		{"options.tui.theme", "dark"},
		{"options.tui.compact_mode", true},
	}
	bases := [][]byte{readA, readB}
	for i, m := range mutations {
		m := m
		base := bases[i]
		go func() {
			defer writeWg.Done()
			tree, err := ktavcodec.DecodeKtav(base)
			if err == nil {
				var jsonBytes []byte
				jsonBytes, err = ktavcodec.ToJSON(tree)
				if err == nil {
					var out []byte
					var jsonOut string
					jsonOut, err = sjson.Set(string(jsonBytes), m.key, m.value)
					if err == nil {
						out, err = ktavcodec.ConvertJSONToKtav([]byte(jsonOut))
						if err == nil {
							err = os.WriteFile(configPath, out, 0o600)
						}
					}
				}
			}
			errCh <- err
		}()
	}
	writeWg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	doc := string(mustDecodeTestKtav(t, fileBytes(t, configPath)))
	themeExists := gjson.Get(doc, "options.tui.theme").Exists()
	compactExists := gjson.Get(doc, "options.tui.compact_mode").Exists()
	require.True(t, themeExists || compactExists, "at least one update must survive")
	require.False(t, themeExists && compactExists,
		"both updates survived WITHOUT a lock — the deterministic lost-update reproduction no longer loses, so this proof test is no longer valid")
}

// TestRemoveConfigFieldBestEffortKtavBoundedByInternalTimeout mirrors
// TestRemoveConfigFieldBestEffort_BoundedByInternalTimeout against a rush.ktav
// target: the best-effort removal injects the internal lock budget, and the
// sidecar acquisition attempt names the legacy-anchored rush.json.lock.
func TestRemoveConfigFieldBestEffortKtavBoundedByInternalTimeout(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.ktav")
	const key = "providers.anthropic.oauth"
	seed := `{"providers":{"anthropic":{"oauth":{"access_token":"secret"}}}}`
	require.NoError(t, os.WriteFile(configPath, testKtavBytes(t, seed), 0o600))

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
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*filelock.FileLock, error) {
		acquireCalls++
		_, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline, "lock acquisition must receive a deadline")
		require.ErrorIs(t, ctx.Err(), context.Canceled, "lock acquisition must receive a canceled context")
		require.Equal(t, normalizeReloadPath(configWriteLockAnchor(configPath))+".lock", path,
			"the sidecar lock must be anchored on the legacy JSON path")
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

	doc := string(mustDecodeTestKtav(t, fileBytes(t, configPath)))
	require.True(t, gjson.Get(doc, key).Exists(),
		"key must still be present after controlled lock acquisition failure")
}

// TestRemoveConfigFieldBestEffortKtavPreservesContentWhenExternalLockHeld
// mirrors TestRemoveConfigFieldBestEffort_PreservesContentWhenExternalLockHeld
// for a rush.ktav target: with the legacy-anchored external rush.json.lock
// held, the removal
// is refused and the document is preserved. The external lock handle is
// released via t.Cleanup (before temp-dir cleanup) for Windows hygiene.
func TestRemoveConfigFieldBestEffortKtavPreservesContentWhenExternalLockHeld(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.ktav")
	const key = "providers.anthropic.oauth"
	seed := `{"providers":{"anthropic":{"oauth":{"access_token":"secret"}}}}`
	require.NoError(t, os.WriteFile(configPath, testKtavBytes(t, seed), 0o600))

	externalLock, err := filelock.TryAcquireFileLock(configWriteLockAnchor(configPath) + ".lock")
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

	doc := string(mustDecodeTestKtav(t, fileBytes(t, configPath)))
	require.True(t, gjson.Get(doc, key).Exists(),
		"key must still be present when the external sidecar lock is held")
}

// TestRemoveConfigFieldBestEffortKtavSucceedsQuicklyWhenLockFree mirrors
// TestRemoveConfigFieldBestEffort_SucceedsQuicklyWhenLockFree for a rush.ktav
// target: the free legacy-anchored rush.json.lock is acquired and released exactly once and the
// key is removed from the Ktav document.
func TestRemoveConfigFieldBestEffortKtavSucceedsQuicklyWhenLockFree(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.ktav")
	const key = "providers.anthropic.oauth"
	seed := `{"providers":{"anthropic":{"oauth":{"access_token":"secret"}}}}`
	require.NoError(t, os.WriteFile(configPath, testKtavBytes(t, seed), 0o600))

	store := newTestConfigStore(testStoreOpts{globalDataPath: configPath})
	entered := make(chan string, 1)
	acquired := make(chan string, 1)
	released := make(chan string, 1)
	attempts := 0
	configTestHooks.Lock()
	previousAcquire := configTestHooks.acquireConfigLock
	previousRelease := configTestHooks.releaseConfigLock
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*filelock.FileLock, error) {
		attempts++
		entered <- path
		lock, err := filelock.TryAcquireFileLock(path)
		if err == nil {
			acquired <- path
		}
		return lock, err
	}
	configTestHooks.releaseConfigLock = func(path string, lock *filelock.FileLock) error {
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
	lockPath := normalizeReloadPath(configWriteLockAnchor(configPath)) + ".lock"
	require.Equal(t, lockPath, <-entered)
	require.Equal(t, lockPath, <-acquired)
	require.Equal(t, lockPath, <-released)

	doc := string(mustDecodeTestKtav(t, fileBytes(t, configPath)))
	require.False(t, gjson.Get(doc, key).Exists(),
		"key must have been removed from disk when the lock was immediately available")
}
