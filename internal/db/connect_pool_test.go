package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	require.Error(t, result.conn.Ping(), "ResetPool must close the entry before returning")
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
			require.NoError(t, newConn.Ping())
			require.NoError(t, ReleaseConn(newConn))
			require.Error(t, newConn.Ping())
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

	require.NoError(t, newConn.Ping(), "late releases from the old generation closed the new generation")
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
