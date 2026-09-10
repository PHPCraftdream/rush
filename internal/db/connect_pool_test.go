package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// slowOpenBudget bounds every wait in these tests so a regression (a hang,
// not a failure) still ends the test quickly instead of eating the package's
// timeout. Task #637 exists precisely because a serialized-open regression
// once presented as a whole-package hang.
const slowOpenBudget = 30 * time.Second

func absDBPath(t *testing.T, dataDir string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join(dataDir, "rush.db"))
	require.NoError(t, err)
	return abs
}

// TestConnect_UnrelatedDataDirsDoNotSerializeBehindMigration pins the #637
// fix: a Connect for one dataDir paused mid-open (before its migrations run)
// must NOT block a concurrent Connect for a completely unrelated dataDir.
// Determinism comes from the onOpenNew seam (a channel handshake), not from
// timing guesses: the slow path is provably parked inside connect() before
// the fast path's Connect is even started, so the only way the fast Connect
// completes is if it didn't queue behind the slow one's lock.
func TestConnect_UnrelatedDataDirsDoNotSerializeBehindMigration(t *testing.T) {
	// Hard watchdog, armed FIRST so its disarm-Cleanup runs LAST — after
	// every other cleanup (slowWG waits, seam teardown, ResetPool, even
	// t.TempDir's RemoveAll). If a regression reintroduces global
	// serialization, the test body's bounded selects fail it — but the
	// cleanup path can also wedge (e.g. a blocked open never returning),
	// and a cleanup deadlock would otherwise eat the whole package's
	// timeout. This watchdog crashes the test binary within its budget so
	// the failure stays loud and fast instead.
	watchdogDisarm := make(chan struct{})
	t.Cleanup(func() { close(watchdogDisarm) })
	go func() {
		select {
		case <-watchdogDisarm:
		case <-time.After(2 * slowOpenBudget):
			panic("TestConnect_UnrelatedDataDirsDoNotSerializeBehindMigration exceeded its hard budget (body or cleanup wedge) — unrelated-path Connect serialization regression?")
		}
	}()

	t.Cleanup(func() {
		onOpenNew = nil
		ResetPool()
	})

	slowDir := t.TempDir()
	fastDir := t.TempDir()
	slowAbs := absDBPath(t, slowDir)
	ctx := context.Background()

	slowStarted := make(chan struct{})
	resumeSlow := make(chan struct{})
	var resumeOnce sync.Once

	onOpenNew = func(p string) {
		if p != slowAbs {
			return
		}
		// Park the slow open mid-flight, before openDB/migrations run.
		close(slowStarted)
		select {
		case <-resumeSlow:
		case <-time.After(2 * slowOpenBudget):
			// Fail the budget rather than hang forever. Twice the body's
			// budget so a genuine regression is reported by the body's own
			// Fatal below first; this arm only fires if the plumbing itself
			// is stuck.
			panic("slow-path open seam exceeded its budget; test plumbing is stuck")
		}
	}

	var slowWG sync.WaitGroup
	slowWG.Add(1)
	go func() {
		defer slowWG.Done()
		conn, err := Connect(ctx, slowDir)
		if err != nil {
			return
		}
		_ = conn.PingContext(ctx)
	}()
	t.Cleanup(func() {
		resumeOnce.Do(func() { close(resumeSlow) })
		slowWG.Wait()
	})

	// Deterministic: do not proceed until the slow Connect is provably
	// parked inside connect()'s slow path.
	select {
	case <-slowStarted:
	case <-time.After(slowOpenBudget):
		t.Fatal("slow-path Connect never reached the onOpenNew seam within budget")
	}

	fastDone := make(chan error, 1)
	go func() {
		conn, err := Connect(ctx, fastDir)
		if err == nil {
			_ = conn.PingContext(ctx)
		}
		fastDone <- err
	}()

	// The fast Connect must finish while the slow one is still parked. On a
	// regression (one global lock across the open+migrate work) it queues
	// behind the parked slow open and this select times out.
	select {
	case err := <-fastDone:
		require.NoError(t, err, "Connect for an unrelated dataDir failed")
	case <-time.After(slowOpenBudget):
		t.Fatalf("Connect for an unrelated dataDir was blocked for %s behind a paused open of a different database — unrelated paths are serializing again", slowOpenBudget)
	}

	// Unblock the slow open and let it finish cleanly.
	resumeOnce.Do(func() { close(resumeSlow) })
	slowDone := make(chan struct{})
	go func() {
		slowWG.Wait()
		close(slowDone)
	}()
	select {
	case <-slowDone:
	case <-time.After(slowOpenBudget):
		t.Fatal("slow-path Connect did not finish within budget after being resumed")
	}

	// Close both pooled connections so t.TempDir's RemoveAll can delete
	// the database files (Windows refuses to delete files with open handles).
	require.NoError(t, ReleaseAll(slowDir))
	require.NoError(t, ReleaseAll(fastDir))
}

// TestResetPool_WaitsForConnectBeforeClearingPool proves ResetPool's
// pool-wide barrier: a cache-miss Connect that is still migrating must finish
// before ResetPool snapshots and closes the pool. In particular, ResetPool
// must not return in the gap between that Connect's miss and publication.
func TestResetPool_WaitsForConnectBeforeClearingPool(t *testing.T) {
	dataDir := t.TempDir()
	absPath := absDBPath(t, dataDir)
	resume := make(chan struct{})
	var resumeOnce sync.Once
	openStarted := make(chan struct{})
	connectDone := make(chan struct {
		conn *sql.DB
		err  error
	}, 1)
	resetDone := make(chan struct{})

	onOpenNew = func(path string) {
		if path != absPath {
			return
		}
		close(openStarted)
		<-resume
	}
	t.Cleanup(func() {
		onOpenNew = nil
		resumeOnce.Do(func() { close(resume) })
		ResetPool()
	})

	go func() {
		conn, err := Connect(context.Background(), dataDir)
		connectDone <- struct {
			conn *sql.DB
			err  error
		}{conn: conn, err: err}
	}()
	select {
	case <-openStarted:
	case <-time.After(slowOpenBudget):
		t.Fatal("Connect did not reach the deterministic open seam")
	}

	go func() {
		ResetPool()
		close(resetDone)
	}()

	// Wait for ResetPool to publish its barrier state, rather than relying on
	// a scheduler timing window. It cannot complete while Connect is active.
	deadline := time.After(slowOpenBudget)
	for {
		lifecycleMu.Lock()
		resetting := lifecycleResetting
		lifecycleMu.Unlock()
		if resetting {
			break
		}
		select {
		case <-deadline:
			t.Fatal("ResetPool did not establish its lifecycle barrier")
		case <-time.After(time.Millisecond):
		}
	}

	select {
	case <-resetDone:
		t.Fatal("ResetPool returned while Connect was still in its open lifecycle")
	default:
	}

	resumeOnce.Do(func() { close(resume) })
	result := <-connectDone
	require.NoError(t, result.err)
	require.NotNil(t, result.conn)
	select {
	case <-resetDone:
	case <-time.After(slowOpenBudget):
		t.Fatal("ResetPool did not finish after Connect completed")
	}

	poolMu.Lock()
	_, pooled := pool[absPath]
	poolMu.Unlock()
	require.False(t, pooled, "ResetPool returned with a newly-published entry still pooled")
	require.Error(t, result.conn.PingContext(t.Context()), "ResetPool must close the entry before returning")
}

// TestResetPool_SerializesResettersAheadOfConnect pins reset writer
// preference. A queued second reset must acquire the lifecycle barrier before
// a Connect already waiting behind the first reset can enter its open path.
func TestResetPool_SerializesResettersAheadOfConnect(t *testing.T) {
	dataDir := t.TempDir()
	absPath := absDBPath(t, dataDir)
	seedConn, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)

	firstCloseStarted := make(chan struct{})
	allowFirstClose := make(chan struct{})
	secondBarrierStarted := make(chan struct{})
	allowSecondBarrier := make(chan struct{})
	connectWaiting := make(chan struct{})
	connectOpenStarted := make(chan struct{})
	allowConnectOpen := make(chan struct{})
	reset1Done := make(chan struct{})
	reset2Done := make(chan struct{})
	connectDone := make(chan struct {
		conn *sql.DB
		err  error
	}, 1)

	var (
		firstCloseOnce     sync.Once
		allowFirstOnce     sync.Once
		secondBarrierOnce  sync.Once
		allowSecondOnce    sync.Once
		connectWaitingOnce sync.Once
		connectOpenOnce    sync.Once
		allowConnectOnce   sync.Once
		resetBarrierCalls  atomic.Int32
		workers            sync.WaitGroup
	)

	onCloseEntry = func(path string) {
		if path != absPath {
			return
		}
		firstCloseOnce.Do(func() { close(firstCloseStarted) })
		<-allowFirstClose
	}
	onResetBarrierAcquired = func() {
		if resetBarrierCalls.Add(1) != 2 {
			return
		}
		secondBarrierOnce.Do(func() { close(secondBarrierStarted) })
		<-allowSecondBarrier
	}
	onLifecycleOperationWaiting = func() {
		connectWaitingOnce.Do(func() { close(connectWaiting) })
	}
	onOpenNew = func(path string) {
		if path != absPath {
			return
		}
		connectOpenOnce.Do(func() { close(connectOpenStarted) })
		<-allowConnectOpen
	}

	t.Cleanup(func() {
		allowFirstOnce.Do(func() { close(allowFirstClose) })
		allowSecondOnce.Do(func() { close(allowSecondBarrier) })
		allowConnectOnce.Do(func() { close(allowConnectOpen) })
		workers.Wait()
		onCloseEntry = nil
		onResetBarrierAcquired = nil
		onLifecycleOperationWaiting = nil
		onOpenNew = nil
		ResetPool()
	})

	workers.Add(1)
	go func() {
		defer workers.Done()
		ResetPool()
		close(reset1Done)
	}()
	select {
	case <-firstCloseStarted:
	case <-time.After(slowOpenBudget):
		t.Fatal("first ResetPool did not reach the close seam")
	}

	workers.Add(1)
	go func() {
		defer workers.Done()
		ResetPool()
		close(reset2Done)
	}()

	// Wait until reset2 is provably queued behind reset1 before admitting the
	// Connect contender. This avoids relying on goroutine scheduling order.
	deadline := time.After(slowOpenBudget)
	for {
		lifecycleMu.Lock()
		queuedResetters := lifecycleResetters
		lifecycleMu.Unlock()
		if queuedResetters == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("second ResetPool did not queue behind the first")
		case <-time.After(time.Millisecond):
		}
	}

	workers.Add(1)
	go func() {
		defer workers.Done()
		conn, connectErr := Connect(context.Background(), dataDir)
		connectDone <- struct {
			conn *sql.DB
			err  error
		}{conn: conn, err: connectErr}
	}()
	select {
	case <-connectWaiting:
	case <-time.After(slowOpenBudget):
		t.Fatal("Connect did not queue behind the active reset")
	}

	select {
	case <-connectOpenStarted:
		t.Fatal("Connect entered its open path while the first reset was active")
	default:
	}

	allowFirstOnce.Do(func() { close(allowFirstClose) })
	select {
	case <-reset1Done:
	case <-time.After(slowOpenBudget):
		t.Fatal("first ResetPool did not complete after its close seam resumed")
	}
	select {
	case <-secondBarrierStarted:
	case <-time.After(slowOpenBudget):
		t.Fatal("second ResetPool did not acquire the barrier after the first")
	}

	select {
	case <-connectOpenStarted:
		t.Fatal("Connect entered its open path while the second reset was active")
	default:
	}
	poolMu.Lock()
	_, pooledDuringSecondReset := pool[absPath]
	poolMu.Unlock()
	require.False(t, pooledDuringSecondReset, "pool must remain empty while reset2 owns the barrier")

	allowSecondOnce.Do(func() { close(allowSecondBarrier) })
	select {
	case <-reset2Done:
	case <-time.After(slowOpenBudget):
		t.Fatal("second ResetPool did not complete after its barrier resumed")
	}
	select {
	case <-connectOpenStarted:
	case <-time.After(slowOpenBudget):
		t.Fatal("Connect did not enter its open path after both resets completed")
	}

	poolMu.Lock()
	_, publishedBeforeOpenResumed := pool[absPath]
	poolMu.Unlock()
	require.False(t, publishedBeforeOpenResumed, "Connect published while its deterministic open seam was blocked")

	allowConnectOnce.Do(func() { close(allowConnectOpen) })
	result := <-connectDone
	require.NoError(t, result.err)
	require.NotNil(t, result.conn)
	require.Error(t, seedConn.PingContext(t.Context()), "the first reset must close the original generation")
	require.NoError(t, result.conn.PingContext(t.Context()), "the post-reset generation must remain open")

	poolMu.Lock()
	finalEntry, pooled := pool[absPath]
	poolMu.Unlock()
	require.True(t, pooled, "post-reset Connect must publish one final pool entry")
	require.Same(t, result.conn, finalEntry.db, "final pool entry must own the post-reset handle")

	workers.Wait()
	onCloseEntry = nil
	onResetBarrierAcquired = nil
	onLifecycleOperationWaiting = nil
	onOpenNew = nil
	require.NoError(t, ReleaseConn(result.conn))
}

func TestReleaseConn_OldGenerationCannotReleaseNewGeneration(t *testing.T) {
	t.Cleanup(ResetPool)

	for _, forceReset := range []struct {
		name string
		fn   func(string) error
	}{
		{name: "release-all", fn: ReleaseAll},
		{name: "reset-pool", fn: func(string) error {
			ResetPool()
			return nil
		}},
	} {
		t.Run(forceReset.name, func(t *testing.T) {
			dataDir := t.TempDir()
			oldConn, err := Connect(context.Background(), dataDir)
			require.NoError(t, err)
			require.NoError(t, forceReset.fn(dataDir))

			newConn, err := Connect(context.Background(), dataDir)
			require.NoError(t, err)
			require.NotSame(t, oldConn, newConn)

			// The old handle is the generation-safe release token. Its late
			// release must be ignored after a forced reset, even though the
			// path has been reused.
			require.NoError(t, ReleaseConn(oldConn))
			require.NoError(t, newConn.PingContext(t.Context()))
			require.NoError(t, ReleaseConn(newConn))
			require.Error(t, newConn.PingContext(t.Context()))
		})
	}
}

func TestReleaseConn_ConcurrentLateOldGenerationReleasesAreHarmless(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()
	oldConn, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)
	require.NoError(t, ReleaseAll(dataDir))

	newConn, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)

	const releasers = 32
	start := make(chan struct{})
	errs := make(chan error, releasers)
	var wg sync.WaitGroup
	for range releasers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- ReleaseConn(oldConn)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for releaseErr := range errs {
		require.NoError(t, releaseErr)
	}

	require.NoError(t, newConn.PingContext(t.Context()), "late releases from the old generation closed the new generation")
	require.NoError(t, ReleaseConn(newConn))
}

// TestConnect_ConcurrentSamePathSharesOneEntry pins the invariant #637 must
// not break: N concurrent Connect calls for the SAME dataDir must all return
// the identical writer *sql.DB, backed by exactly one pool entry with
// refCount N — never two independent connections to one database file (the
// SQLITE_NOTADB WAL/header-desync hazard documented at SetMaxOpenConns(1)).
func TestConnect_ConcurrentSamePathSharesOneEntry(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()
	abs := absDBPath(t, dataDir)
	ctx := context.Background()

	const callers = 8
	conns := make([]*sql.DB, callers)
	errs := make([]error, callers)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			conns[i], errs[i] = Connect(ctx, dataDir)
		}()
	}
	close(start)

	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(slowOpenBudget):
		t.Fatal("concurrent same-path Connect calls did not all finish within budget")
	}

	for i := range callers {
		require.NoError(t, errs[i], "caller %d's Connect failed", i)
		require.NotNil(t, conns[i], "caller %d got a nil connection", i)
		if i > 0 {
			require.Same(t, conns[0], conns[i],
				"caller %d got a different *sql.DB for the same dataDir — two live writer connections to one database file is the SQLITE_NOTADB hazard", i)
		}
	}

	poolMu.Lock()
	entry, ok := pool[abs]
	refCount := 0
	if ok {
		refCount = entry.refCount
	}
	poolMu.Unlock()

	require.True(t, ok, "no pool entry for the dataDir after all Connects returned")
	require.Equal(t, callers, refCount,
		"pool entry refCount must equal the number of concurrent Connect calls")

	require.NoError(t, ReleaseAll(dataDir))
}

func TestPathLocks_ReclaimedAfterManyUniquePaths(t *testing.T) {
	t.Cleanup(ResetPool)

	root := t.TempDir()
	const paths = 48
	ctx := context.Background()
	for i := range paths {
		dataDir := filepath.Join(root, fmt.Sprintf("db-%03d", i))
		require.NoError(t, os.Mkdir(dataDir, 0o755))

		_, err := Connect(ctx, dataDir)
		require.NoError(t, err)
		require.NoError(t, Release(dataDir))

		abs := absDBPath(t, dataDir)
		pathLocksMu.Lock()
		_, stillRegistered := pathLocks[abs]
		pathLocksMu.Unlock()
		require.False(t, stillRegistered, "released path %q remains in the path-lock registry", abs)
	}
}

func TestConnectRelease_ConcurrentGenerationsReclaimPathLock(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()
	ctx := context.Background()
	const (
		callers = 8
		rounds  = 4
	)

	for round := range rounds {
		start := make(chan struct{})
		conns := make([]*sql.DB, callers)
		errs := make([]error, callers)
		var connectWG sync.WaitGroup
		for i := range callers {
			connectWG.Add(1)
			go func(i int) {
				defer connectWG.Done()
				<-start
				conns[i], errs[i] = Connect(ctx, dataDir)
			}(i)
		}
		close(start)
		connectWG.Wait()

		for i := range callers {
			require.NoError(t, errs[i], "round %d Connect caller %d failed", round, i)
			require.NotNil(t, conns[i], "round %d Connect caller %d returned nil", round, i)
			require.Same(t, conns[0], conns[i], "round %d opened more than one connection", round)
		}

		releaseStart := make(chan struct{})
		releaseErrs := make([]error, callers)
		var releaseWG sync.WaitGroup
		for i := range callers {
			releaseWG.Add(1)
			go func(i int) {
				defer releaseWG.Done()
				<-releaseStart
				releaseErrs[i] = Release(dataDir)
			}(i)
		}
		close(releaseStart)
		releaseWG.Wait()

		for i := range callers {
			require.NoError(t, releaseErrs[i], "round %d Release caller %d failed", round, i)
		}

		abs := absDBPath(t, dataDir)
		poolMu.Lock()
		_, inPool := pool[abs]
		poolMu.Unlock()
		pathLocksMu.Lock()
		_, inPathLocks := pathLocks[abs]
		pathLocksMu.Unlock()
		require.False(t, inPool, "round %d left a pooled entry", round)
		require.False(t, inPathLocks, "round %d left a path-lock entry", round)
	}
}

func TestPathLocks_RetainedForWaiterDuringFinalRelease(t *testing.T) {
	dataDir := t.TempDir()
	abs := absDBPath(t, dataDir)
	ctx := context.Background()
	first, err := Connect(ctx, dataDir)
	require.NoError(t, err)

	acquired := make(chan struct{}, 2)
	closeStarted := make(chan struct{})
	allowClose := make(chan struct{})
	var closeStartedOnce sync.Once
	var allowCloseOnce sync.Once
	onPathLockAcquired = func(path string) {
		if path == abs {
			acquired <- struct{}{}
		}
	}
	onCloseEntry = func(path string) {
		if path != abs {
			return
		}
		closeStartedOnce.Do(func() { close(closeStarted) })
		<-allowClose
	}
	t.Cleanup(func() {
		onPathLockAcquired = nil
		onCloseEntry = nil
		allowCloseOnce.Do(func() { close(allowClose) })
		ResetPool()
	})

	releaseDone := make(chan error, 1)
	go func() { releaseDone <- Release(dataDir) }()
	select {
	case <-closeStarted:
	case <-time.After(slowOpenBudget):
		t.Fatal("final Release did not reach the close seam")
	}

	secondDone := make(chan struct {
		conn *sql.DB
		err  error
	}, 1)
	go func() {
		conn, connectErr := Connect(ctx, dataDir)
		secondDone <- struct {
			conn *sql.DB
			err  error
		}{conn: conn, err: connectErr}
	}()

	for range 2 {
		select {
		case <-acquired:
		case <-time.After(slowOpenBudget):
			t.Fatal("Connect/Release did not acquire path-lock references")
		}
	}

	pathLocksMu.Lock()
	lock, registered := pathLocks[abs]
	refs := 0
	if registered {
		refs = lock.refs
	}
	pathLocksMu.Unlock()
	require.True(t, registered, "path lock was removed while a waiter was active")
	require.Equal(t, 3, refs, "path lock should hold pool, final Release, and waiter references")

	allowCloseOnce.Do(func() { close(allowClose) })
	require.NoError(t, <-releaseDone)
	result := <-secondDone
	require.NoError(t, result.err)
	require.NotNil(t, result.conn)
	require.NotSame(t, first, result.conn, "a waiter must open a new generation after final Release")

	onPathLockAcquired = nil
	require.NoError(t, Release(dataDir))

	pathLocksMu.Lock()
	_, registered = pathLocks[abs]
	pathLocksMu.Unlock()
	require.False(t, registered, "path lock was not reclaimed after the waiter released")
}
