package config

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TestSetConfigFields_TwoStoresSameFile_BothUpdatesSurvive simulates two
// parallel rush processes writing DIFFERENT keys to the SAME config file at
// the same time. Each ConfigStore owns a private diskWriteMu (just as two
// separate OS processes would), so in-process serialisation cannot help —
// only the inter-process sidecar lock (path+".lock", via session.FileLock)
// can prevent the lost update. session.FileLock is backed by flock/LockFileEx,
// which are per-open-file-description, so two opens inside one test process
// contend on the lock exactly as two real processes would.
//
// Without the sidecar lock (the pre-fix code, which only had diskWriteMu),
// both stores would read the same "{}", each apply only its own key, and the
// second atomicWriteFile rename would erase the first store's key — a silent
// cross-process lost update. Run with -race.
func TestSetConfigFields_TwoStoresSameFile_BothUpdatesSurvive(t *testing.T) {
	// Keep this global file-publication test out of package parallelism.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
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
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*session.FileLock, error) {
		id := int(acquireCalls.Add(1))
		acquireEntered <- id
		if id == 2 {
			close(secondAcquireReady)
			<-allowSecondProbe
			probe, probeErr := session.TryAcquireFileLock(path)
			secondProbeResult <- probeErr
			if probeErr == nil {
				acquireAcquired <- id
				return probe, nil
			}
			<-releaseFirst
			lock, err := session.AcquireFileLockContext(ctx, path)
			if err == nil {
				acquireAcquired <- id
			}
			return lock, err
		}
		lock, err := session.AcquireFileLockContext(ctx, path)
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

	// Pause the first publication at the commit boundary so the second writer
	// must encounter the sidecar lock in a deterministic state.
	const iterations = 1
	for i := range iterations {
		require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

		// Two independent stores sharing the SAME globalDataPath — two
		// separate in-process mutexes, like two rush processes sharing one
		// config file. workingDir is "" so autoReload is a no-op (keeps the
		// test free of provider/network work).
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

		probe, probeErr := session.TryAcquireFileLock(configPath + ".lock")
		if probeErr == nil {
			_ = probe.Release()
			require.FailNow(t, "the first writer must hold the sidecar lock while its commit is paused")
		}
		var contended *session.ErrLockContended
		require.ErrorAs(t, probeErr, &contended)

		allowSecondProbeOnce.Do(func() { close(allowSecondProbe) })
		secondProbeErr := <-secondProbeResult
		require.ErrorAs(t, secondProbeErr, &contended)
		releaseFirstCommit.Do(func() { close(releaseFirst) })
		require.Equal(t, 2, <-acquireAcquired)
		wg.Wait()

		require.NoError(t, err1, "iter %d: store1 write failed", i)
		require.NoError(t, err2, "iter %d: store2 write failed", i)

		data, rerr := os.ReadFile(configPath)
		require.NoError(t, rerr)
		require.True(t, gjson.Get(string(data), "options.tui.theme").Exists(),
			"iter %d: store1's key (theme) was lost — cross-process lost update", i)
		require.True(t, gjson.Get(string(data), "options.tui.compact_mode").Exists(),
			"iter %d: store2's key (compact_mode) was lost — cross-process lost update", i)
	}
}

// TestUnprotectedRMW_DeterministicallyLosesUpdate is the proof that the bug
// class fixed by withConfigWriteLock is real. It reproduces the pre-fix body
// of SetConfigFields MINUS any lock (diskWriteMu is process-private and thus
// irrelevant across processes): two "processes" each read-modify-write the
// same file, synchronised by a barrier so BOTH finish reading BEFORE either
// writes. Each therefore reads "{}" and writes a file containing only its own
// key; the second write overwrites the first — exactly one update survives.
// This is the cross-process lost update that diskWriteMu alone cannot prevent.
func TestUnprotectedRMW_DeterministicallyLosesUpdate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

	// Phase 1: both read the file BEFORE any write (barrier). Both see "{}".
	var readA, readB []byte
	var readWg sync.WaitGroup
	readWg.Add(2)
	go func() {
		defer readWg.Done()
		d, err := os.ReadFile(configPath)
		if err != nil || len(d) == 0 {
			d = []byte("{}")
		}
		readA = d
	}()
	go func() {
		defer readWg.Done()
		d, err := os.ReadFile(configPath)
		if err != nil || len(d) == 0 {
			d = []byte("{}")
		}
		readB = d
	}()
	readWg.Wait()

	// Phase 2: both write from their own (stale, identical) read. Each write
	// is a full overwrite, so the second clobbers the first.
	//
	// Errors are collected on a channel rather than asserted with
	// require.NoError inside the goroutines themselves: testing.T's
	// FailNow/Fatal (which require.NoError calls on failure) is documented
	// to be safe only from the test's own goroutine — calling it from a
	// goroutine spawned by the test is undefined behavior and can hang or
	// misbehave. So each goroutine only sends its error (nil on success)
	// and every assertion happens back in the main test goroutine below.
	errCh := make(chan error, 2)
	var writeWg sync.WaitGroup
	writeWg.Add(2)
	go func() {
		defer writeWg.Done()
		out, err := sjson.Set(string(readA), "options.tui.theme", "dark")
		if err != nil {
			errCh <- err
			return
		}
		errCh <- os.WriteFile(configPath, []byte(out), 0o600)
	}()
	go func() {
		defer writeWg.Done()
		out, err := sjson.Set(string(readB), "options.tui.compact_mode", true)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- os.WriteFile(configPath, []byte(out), 0o600)
	}()
	writeWg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	themeExists := gjson.Get(string(data), "options.tui.theme").Exists()
	compactExists := gjson.Get(string(data), "options.tui.compact_mode").Exists()
	require.True(t, themeExists || compactExists, "at least one update must survive")
	require.False(t, themeExists && compactExists,
		"both updates survived WITHOUT a lock — the deterministic lost-update reproduction no longer loses, so this proof test is no longer valid")
}
