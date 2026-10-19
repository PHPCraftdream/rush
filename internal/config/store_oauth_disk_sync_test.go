package config

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func diskSyncPair(t *testing.T, id string) (codexRefreshFixture, *ConfigStore) {
	t.Helper()
	f := task1292Fixture(t, id)
	return f, task1292Store(t, f.path, f.used)
}

func diskSyncLive(t *testing.T, s *ConfigStore, id string) *oauth.Token {
	t.Helper()
	pc, ok := s.Config().Providers.Get(id)
	require.True(t, ok)
	return pc.OAuthToken
}

func TestOAuthDiskSync01SiblingRefresh(t *testing.T) {
	// Revert-check: removing polling adoption leaves A on the predecessor.
	for _, id := range []string{task1292Provider, "copilot"} {
		t.Run(id, func(t *testing.T) {
			f, b := diskSyncPair(t, id)
			if id == task1292Provider {
				task1292Descriptor(t, false, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
			} else {
				old := copilotRefreshTokenFn
				copilotRefreshTokenFn = func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil }
				t.Cleanup(func() { copilotRefreshTokenFn = old })
				joinOAuthJournalTestStoresBeforeRestore(t)
			}
			idle := hookOAuthJournalIdle(t, b)
			require.NoError(t, b.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
			if id == task1292Provider {
				waitOAuthJournalIdle(t, idle)
			}
			var st oauthDiskSyncState
			f.store.syncOAuthFromDisk(context.Background(), &st)
			require.Equal(t, diskSyncLive(t, b, id), diskSyncLive(t, f.store, id))
			gen := f.store.Generation()
			f.store.syncOAuthFromDisk(context.Background(), &st)
			require.Equal(t, gen, f.store.Generation())
		})
	}
}

func TestOAuthDiskSync02OwnedPredecessor(t *testing.T) {
	// Revert-check: bypassing the arbiter republishes a burned predecessor.
	task1292Descriptor(t, false, nil)
	for _, outbox := range []bool{false, true} {
		f, _ := diskSyncPair(t, task1292Provider)
		burned := *f.used.OAuthToken
		burned.ExpiresAt = f.next.ExpiresAt + 100
		record := &oauthJournalRecord{Token: f.next, BurnedRefreshTokenSHA256: []string{oauthRefreshTokenSHA256(&burned)}}
		if outbox {
			f.store.installOAuthOutbox(f.path, ScopeGlobal, task1292Provider, record)
		}
		require.NoError(t, f.store.applyOAuthTokenWithPendingPredecessor(task1292Provider, f.used.OAuthToken, f.next, f.used.OAuthToken, true, record))
		task1292WriteDisk(t, f.path, task1292Provider, &burned, "")
		var st oauthDiskSyncState
		f.store.syncOAuthFromDisk(context.Background(), &st)
		require.Equal(t, f.next, diskSyncLive(t, f.store, task1292Provider))
		require.False(t, st.settled)
	}
}

func TestOAuthDiskSync03UncertainNoLogout(t *testing.T) {
	// Revert-check: absent OAuth settles the stat gate without clearing live credentials.
	task1292Descriptor(t, false, nil)
	for _, data := range []string{"", "{", "{}", `{"providers":{"fixture-task1292-rotating":{"oauth":null}}}`} {
		f, _ := diskSyncPair(t, task1292Provider)
		if data == "" {
			require.NoError(t, os.Remove(f.path))
		} else {
			require.NoError(t, os.WriteFile(f.path, []byte(data), 0o600))
		}
		var st oauthDiskSyncState
		f.store.syncOAuthFromDisk(context.Background(), &st)
		require.Equal(t, f.used.OAuthToken, diskSyncLive(t, f.store, task1292Provider))
		require.Zero(t, f.store.Generation())
		if data == "{}" || data == `{"providers":{"fixture-task1292-rotating":{"oauth":null}}}` {
			require.True(t, st.settled, "absent disk OAuth must settle the full check")
		}
	}
}

func TestOAuthDiskSync04EqualNoPublish(t *testing.T) {
	// Revert-check: publishing equal bytes increments the generation.
	f, _ := diskSyncPair(t, "copilot")
	valid := *f.used.OAuthToken
	valid.ExpiresAt = time.Now().Add(time.Hour).Unix()
	require.NoError(t, f.store.applyOAuthToken("copilot", f.used.OAuthToken, &valid))
	task1292WriteDisk(t, f.path, "copilot", &valid, valid.AccessToken)
	base := f.store.Generation()
	var st oauthDiskSyncState
	f.store.syncOAuthFromDisk(context.Background(), &st)
	require.True(t, st.settled)
	require.Equal(t, base, f.store.Generation())
	// Change bytes in place while preserving every stat-gate input.
	before, err := os.Stat(f.path)
	require.NoError(t, err)
	data, err := os.ReadFile(f.path)
	require.NoError(t, err)
	disk := valid
	disk.AccessToken = "X" + disk.AccessToken[1:]
	changed := bytes.ReplaceAll(data, []byte(f.used.OAuthToken.AccessToken), []byte(disk.AccessToken))
	require.NotEqual(t, data, changed)
	require.Len(t, changed, len(data))
	require.NoError(t, os.WriteFile(f.path, changed, 0o600))
	require.NoError(t, os.Chtimes(f.path, before.ModTime(), before.ModTime()))
	after, err := os.Stat(f.path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	require.Equal(t, before.Size(), after.Size())
	require.True(t, before.ModTime().Equal(after.ModTime()))
	for st.tick < 11 {
		f.store.syncOAuthFromDisk(context.Background(), &st)
		require.Equal(t, &valid, diskSyncLive(t, f.store, "copilot"))
		require.Equal(t, base, f.store.Generation())
	}
	f.store.syncOAuthFromDisk(context.Background(), &st)
	require.EqualValues(t, 12, st.tick)
	require.Equal(t, &disk, diskSyncLive(t, f.store, "copilot"))
	require.Equal(t, base+1, f.store.Generation())
	// Settle the newly published generation, then invalidate only generation.
	f.store.syncOAuthFromDisk(context.Background(), &st)
	require.True(t, st.settled)
	require.NoError(t, f.store.applyOAuthToken("copilot", &disk, &valid))
	f.store.syncOAuthFromDisk(context.Background(), &st)
	require.EqualValues(t, 14, st.tick)
	require.Equal(t, &disk, diskSyncLive(t, f.store, "copilot"))
	require.Equal(t, base+3, f.store.Generation())
}

func TestOAuthDiskSync05ExpiryOrdering(t *testing.T) {
	// Revert-check: changing legacy ordering rejects an earlier valid sibling token.
	task1292Descriptor(t, false, nil)
	for _, id := range []string{task1292Provider, "copilot"} {
		for _, expired := range []bool{false, true} {
			f, _ := diskSyncPair(t, id)
			live := *f.next
			now := time.Now()
			live.ExpiresAt = now.Add(2 * time.Hour).Unix()
			if expired {
				live.ExpiresAt = now.Add(-2 * time.Hour).Unix()
			}
			require.NoError(t, f.store.applyOAuthToken(id, f.used.OAuthToken, &live))
			gen := f.store.Generation()
			disk := *f.next
			disk.AccessToken = "fixture-earlier-access"
			disk.ExpiresAt = now.Add(time.Hour).Unix()
			if expired {
				disk.ExpiresAt = now.Add(-time.Hour).Unix()
				require.Less(t, live.ExpiresAt, disk.ExpiresAt)
				require.Less(t, disk.ExpiresAt, now.Unix())
			} else {
				require.Greater(t, disk.ExpiresAt, now.Unix())
				require.Less(t, disk.ExpiresAt, live.ExpiresAt)
			}
			task1292WriteDisk(t, f.path, id, &disk, disk.AccessToken)
			var st oauthDiskSyncState
			f.store.syncOAuthFromDisk(context.Background(), &st)
			if id == "copilot" && !expired {
				require.Equal(t, &disk, diskSyncLive(t, f.store, id))
				require.Equal(t, disk.ExpiresAt, diskSyncLive(t, f.store, id).ExpiresAt)
				require.Equal(t, gen+1, f.store.Generation())
			} else {
				require.Equal(t, &live, diskSyncLive(t, f.store, id))
				require.Equal(t, live.ExpiresAt, diskSyncLive(t, f.store, id).ExpiresAt)
				require.Equal(t, gen, f.store.Generation())
				require.False(t, st.settled)
			}
		}
	}
}

func TestOAuthDiskSync06ForeignWALRetry(t *testing.T) {
	// Revert-check: marking deferred disk settled prevents adoption after WAL removal.
	task1292Descriptor(t, false, nil)
	f, _ := diskSyncPair(t, task1292Provider)
	task1292WriteDisk(t, f.path, task1292Provider, f.next, "")
	record := oauthJournalRecord{Token: f.next}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	wal := oauthJournalPath(f.path, task1292Provider)
	require.NoError(t, os.WriteFile(wal, data, 0o600))
	var st oauthDiskSyncState
	f.store.syncOAuthFromDisk(context.Background(), &st)
	require.Equal(t, f.used.OAuthToken, diskSyncLive(t, f.store, task1292Provider))
	require.False(t, st.settled)
	require.NoError(t, os.Remove(wal))
	f.store.syncOAuthFromDisk(context.Background(), &st)
	require.Equal(t, f.next, diskSyncLive(t, f.store, task1292Provider))
}

func TestOAuthDiskSync07InflightBounded(t *testing.T) {
	// Revert-check: taking refresh slot or sidecar in polling blocks the tick.
	f, b := diskSyncPair(t, task1292Provider)
	entered, release := make(chan struct{}), make(chan struct{})
	task1292Descriptor(t, false, func(ctx context.Context, _ *http.Client, _ string) (*oauth.Token, error) {
		close(entered)
		select {
		case <-release:
			return f.next, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	idle := hookOAuthJournalIdle(t, b)
	result := task1292Start(t, b, f.used)
	awaitCodexSignal(t, entered)
	done := make(chan struct{})
	go func() { defer close(done); var st oauthDiskSyncState; b.syncOAuthFromDisk(context.Background(), &st) }()
	awaitCodexSignal(t, done)
	close(release)
	require.NoError(t, <-result)
	waitOAuthJournalIdle(t, idle)
	var st oauthDiskSyncState
	f.store.syncOAuthFromDisk(context.Background(), &st)
	require.Equal(t, f.next, diskSyncLive(t, f.store, task1292Provider))
}

func TestOAuthDiskSync08SampledCAS(t *testing.T) {
	// Revert-check: rereading live in the shared helper overwrites a concurrent publication.
	task1292Descriptor(t, false, nil)
	f, _ := diskSyncPair(t, task1292Provider)
	task1292WriteDisk(t, f.path, task1292Provider, f.next, "")
	newer := *f.next
	newer.AccessToken, newer.RefreshToken = "fixture-C-access", "fixture-C-refresh"
	old := oauthJournalReadFile
	oauthJournalReadFile = func(path string) ([]byte, error) {
		require.NoError(t, f.store.applyOAuthToken(task1292Provider, f.used.OAuthToken, &newer))
		return old(path)
	}
	t.Cleanup(func() { oauthJournalReadFile = old })
	var st oauthDiskSyncState
	f.store.syncOAuthFromDisk(context.Background(), &st)
	require.Equal(t, &newer, diskSyncLive(t, f.store, task1292Provider))
	require.False(t, st.settled)
}

func TestOAuthDiskSync09LifecycleNoIO(t *testing.T) {
	// Revert-check: duplicate starts spawn extra workers or no-token ticks stat disk.
	s := newTestConfigStore(testStoreOpts{globalDataPath: "fixture-unused", config: &Config{Providers: csync.NewMap[string, ProviderConfig]()}})
	var waits, stats atomic.Int32
	entered := make(chan struct{}, 2)
	cancelled, release, exited, stopped := make(chan struct{}, 2), make(chan struct{}), make(chan struct{}, 2), make(chan struct{})
	var releaseOnce, stopOnce sync.Once
	releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
	startStop := func() { stopOnce.Do(func() { go func() { s.stopOAuthJournalWorkers(); close(stopped) }() }) }
	oldWait, oldStat := oauthDiskSyncWait, oauthDiskSyncStat
	oauthDiskSyncWait = func(ctx context.Context, _ time.Duration) error {
		defer func() { exited <- struct{}{} }()
		waits.Add(1)
		entered <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		<-release
		return ctx.Err()
	}
	oauthDiskSyncStat = func(string) (os.FileInfo, error) { stats.Add(1); return nil, os.ErrNotExist }
	// Register before assertions; even an untracked mutant is released and joined
	// through the hook-exit signal before global hooks are restored.
	t.Cleanup(func() {
		releaseWorker()
		startStop()
		awaitCodexSignal(t, stopped)
		awaitCodexSignal(t, exited)
		oauthDiskSyncWait, oauthDiskSyncStat = oldWait, oldStat
	})
	s.StartOAuthDiskSync()
	s.StartOAuthDiskSync()
	awaitCodexSignal(t, entered)
	startStop()
	awaitCodexSignal(t, cancelled)
	returnedEarly := false
	select {
	case <-stopped:
		returnedEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	releaseWorker()
	awaitCodexSignal(t, stopped)
	require.False(t, returnedEarly, "shutdown returned before blocked poller was released")
	s.StartOAuthDiskSync()
	require.EqualValues(t, 1, waits.Load())
	var st oauthDiskSyncState
	// A separate uncancelled store proves the no-token early exit.
	empty := newTestConfigStore(testStoreOpts{globalDataPath: s.globalDataPath, config: s.Config()})
	empty.syncOAuthFromDisk(context.Background(), &st)
	require.Zero(t, stats.Load())
	stoppedStore := newTestConfigStore(testStoreOpts{globalDataPath: s.globalDataPath})
	stoppedStore.stopOAuthJournalWorkers()
	stoppedStore.StartOAuthDiskSync()
	require.False(t, stoppedStore.oauthDiskSyncStarted)
	noPath := newTestConfigStore(testStoreOpts{})
	noPath.StartOAuthDiskSync()
	require.Nil(t, noPath.oauthWorkerCtx)
}

func TestOAuthDiskSync10ProviderBoundary(t *testing.T) {
	// Revert-check: iterating disk providers creates or replaces unknown/nil providers.
	f, _ := diskSyncPair(t, "copilot")
	f.store.SetProviderRuntimeConfig("unknown", ProviderConfig{ID: "unknown", OAuthToken: f.used.OAuthToken})
	f.store.SetProviderRuntimeConfig("hyper", ProviderConfig{ID: "hyper"})
	data, err := json.Marshal(map[string]any{"providers": map[string]any{
		"unknown": map[string]any{"oauth": f.next}, "hyper": map[string]any{"oauth": f.next}, "openai-codex": map[string]any{"oauth": f.next},
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.path, data, 0o600))
	gen := f.store.Generation()
	var st oauthDiskSyncState
	f.store.syncOAuthFromDisk(context.Background(), &st)
	require.Equal(t, gen, f.store.Generation())
	require.Equal(t, f.used.OAuthToken, diskSyncLive(t, f.store, "unknown"))
	require.Nil(t, diskSyncLive(t, f.store, "hyper"))
	_, exists := f.store.Config().Providers.Get("openai-codex")
	require.False(t, exists)
	require.Equal(t, f.used.OAuthToken, diskSyncLive(t, f.store, "copilot"))
}

func TestOAuthDiskSync11Jitter(t *testing.T) {
	// Revert-check: an incorrect jitter range escapes the four-to-six-second bounds.
	require.Equal(t, 5*time.Second, oauthDiskSyncInterval)
	for range 1000 {
		delay := oauthDiskSyncDelay()
		require.GreaterOrEqual(t, delay, 4*time.Second)
		require.LessOrEqual(t, delay, 6*time.Second)
	}
}

func diskSyncAwaitDelay(t *testing.T, ch <-chan time.Duration) time.Duration {
	t.Helper()
	select {
	case delay := <-ch:
		return delay
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for disk sync delay")
		return 0
	}
}

func TestOAuthDiskSync12IdleConvergence(t *testing.T) {
	// Revert-check: without the worker tick sibling rotation never reaches idle memory.
	f, b := diskSyncPair(t, task1292Provider)
	var calls atomic.Int32
	task1292Descriptor(t, false, func(context.Context, *http.Client, string) (*oauth.Token, error) { calls.Add(1); return f.next, nil })
	idle := hookOAuthJournalIdle(t, b)
	first, permit, second := make(chan time.Duration, 1), make(chan struct{}), make(chan time.Duration, 1)
	old := oauthDiskSyncWait
	var waits atomic.Int32
	oauthDiskSyncWait = func(ctx context.Context, delay time.Duration) error {
		if waits.Add(1) == 1 {
			first <- delay
			select {
			case <-permit:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		second <- delay
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { oauthDiskSyncWait = old })
	joinOAuthJournalTestStoresBeforeRestore(t)
	f.store.StartOAuthDiskSync()
	initial := diskSyncAwaitDelay(t, first)
	require.GreaterOrEqual(t, initial, time.Duration(0))
	require.Less(t, initial, oauthDiskSyncInterval)
	require.NoError(t, b.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	waitOAuthJournalIdle(t, idle)
	close(permit)
	delay := diskSyncAwaitDelay(t, second)
	require.GreaterOrEqual(t, delay, 4*time.Second)
	require.LessOrEqual(t, delay, 6*time.Second)
	require.Equal(t, f.next, diskSyncLive(t, f.store, task1292Provider))
	require.EqualValues(t, 1, calls.Load())
	f.store.stopOAuthJournalWorkers()
}

func TestOAuthDiskSync13StopJoinsPollerAndJournalWorkers(t *testing.T) {
	// Revert-check: a StopOAuthDiskSync that neither cancels nor joins leaves
	// the poller and journal workers running past its return. Both workers
	// park AFTER cancellation until explicitly released, so the PUBLIC
	// StopOAuthDiskSync must not return before either join completes
	// (oauth-poller-stop#2). Catches: removing the poller join or the
	// oauthWorkerWG.Wait from StopOAuthDiskSync lets it return while a
	// worker is still parked → returnedEarly fires.
	s := newTestConfigStore(testStoreOpts{globalDataPath: "fixture-unused", config: &Config{Providers: csync.NewMap[string, ProviderConfig]()}})
	var waits atomic.Int32
	pollerEntered, pollerCancelled, pollerExited := make(chan struct{}, 2), make(chan struct{}, 2), make(chan struct{}, 2)
	workerEntered, workerCancelled, workerExited := make(chan struct{}, 2), make(chan struct{}, 2), make(chan struct{}, 2)
	releasePollerCh := make(chan struct{})
	releaseWorkerCh := make(chan struct{})
	var releaseOnce sync.Once
	releaseBoth := func() {
		releaseOnce.Do(func() { close(releasePollerCh); close(releaseWorkerCh) })
	}
	oldWait := oauthDiskSyncWait
	oldBefore, oldDone := oauthJournalBeforeWorker, oauthJournalWorkerDone
	oauthDiskSyncWait = func(ctx context.Context, _ time.Duration) error {
		defer func() { pollerExited <- struct{}{} }()
		waits.Add(1)
		pollerEntered <- struct{}{}
		<-ctx.Done()
		pollerCancelled <- struct{}{}
		<-releasePollerCh
		return ctx.Err()
	}
	oauthJournalBeforeWorker = func(ctx context.Context) {
		workerEntered <- struct{}{}
		<-ctx.Done()
		workerCancelled <- struct{}{}
		<-releaseWorkerCh
		workerExited <- struct{}{}
	}
	oauthJournalWorkerDone = func(_ *ConfigStore, _ string) {}
	// Register before assertions: even a failing body releases both workers
	// and joins them through the hook-exit signals before the global hooks
	// are restored.
	t.Cleanup(func() {
		releaseBoth()
		s.stopOAuthJournalWorkers()
		awaitCodexSignal(t, pollerExited)
		awaitCodexSignal(t, workerExited)
		oauthDiskSyncWait = oldWait
		oauthJournalBeforeWorker, oauthJournalWorkerDone = oldBefore, oldDone
	})
	s.StartOAuthDiskSync()
	s.startOAuthJournalWorker(ScopeGlobal, task1292Provider)
	awaitCodexSignal(t, pollerEntered)
	awaitCodexSignal(t, workerEntered)
	stopped := make(chan struct{})
	go func() { defer close(stopped); s.StopOAuthDiskSync() }()
	awaitCodexSignal(t, pollerCancelled)
	awaitCodexSignal(t, workerCancelled)
	returnedEarly := false
	select {
	case <-stopped:
		returnedEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	require.False(t, returnedEarly, "StopOAuthDiskSync returned while a cancelled worker was still parked")
	releaseBoth()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("StopOAuthDiskSync did not return after both workers were released")
	}
	require.EqualValues(t, 1, waits.Load())
	select {
	case <-pollerEntered:
		t.Fatal("poller ticked after StopOAuthDiskSync returned")
	case <-time.After(100 * time.Millisecond):
	}
	s.StopOAuthDiskSync() // Idempotent.
	require.False(t, s.OAuthDiskSyncRunning())
	s.StartOAuthDiskSync() // Refused after a stop.
	s.startOAuthJournalWorker(ScopeGlobal, task1292Provider)
	s.oauthWorkerMu.Lock()
	workers := len(s.oauthWorkers)
	s.oauthWorkerMu.Unlock()
	require.EqualValues(t, 1, waits.Load(), "start after stop must be refused")
	require.Zero(t, workers, "journal worker start after stop must be refused")
	require.False(t, s.OAuthDiskSyncRunning())
}

func TestOAuthDiskSync14StopFlushesPendingRotationOutbox(t *testing.T) {
	// Revert-check: a StopOAuthDiskSync without the post-join outbox drain
	// leaves the rotation WAL unwritten; the provider has already burned the
	// old refresh token, so the next process would force a re-login.
	task1292Descriptor(t, false, nil)
	f, _ := diskSyncPair(t, task1292Provider)
	record := &oauthJournalRecord{PredecessorRefreshTokenSHA256: oauthRotationDigest(f.used.OAuthToken, f.next), Token: f.next}
	entry := f.store.installOAuthOutbox(f.path, ScopeGlobal, task1292Provider, record)
	f.store.StopOAuthDiskSync()
	flushed, err := readOAuthJournal(oauthJournalPath(f.path, task1292Provider))
	require.NoError(t, err)
	require.NotNil(t, flushed, "StopOAuthDiskSync must flush a pending rotation outbox to the journal WAL")
	require.Equal(t, &entry.record, flushed)
}

func TestOAuthDiskSync15StopJoinsBlockedJournalWorker(t *testing.T) {
	// Revert-check: a StopOAuthDiskSync that cancels without joining
	// returns while the journal worker is still blocked after cancellation.
	s := newTestConfigStore(testStoreOpts{globalDataPath: "fixture-unused", config: &Config{Providers: csync.NewMap[string, ProviderConfig]()}})
	entered, cancelled := make(chan struct{}, 2), make(chan struct{}, 2)
	var releaseOnce sync.Once
	release := make(chan struct{})
	releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
	oldBefore := oauthJournalBeforeWorker
	oauthJournalBeforeWorker = func(ctx context.Context) {
		entered <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		<-release
	}
	// Register before assertions: even a failing body releases the worker
	// through the hook before the global hook is restored.
	t.Cleanup(func() {
		releaseWorker()
		s.stopOAuthJournalWorkers()
		oauthJournalBeforeWorker = oldBefore
	})
	s.startOAuthJournalWorker(ScopeGlobal, task1292Provider)
	awaitCodexSignal(t, entered)
	stopped := make(chan struct{})
	go func() { defer close(stopped); s.StopOAuthDiskSync() }()
	awaitCodexSignal(t, cancelled)
	returnedEarly := false
	select {
	case <-stopped:
		returnedEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	require.False(t, returnedEarly, "StopOAuthDiskSync returned before the blocked journal worker was released")
	releaseWorker()
	awaitCodexSignal(t, stopped)
}
