// Durable external-driver marker coverage (docs/reviews/2026-09-29-async-
// phase4-round1-rerun-design.md, Problem 2). Real SQLite, one data dir shared
// by several stores standing in for several processes: a store's own host and
// any sibling store's host in this test process are alive without probing
// (IsOwnHostID); SimulateCrashForTest makes a store's host provably dead.
package session

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// nStores opens n independent connections to ONE database file/data dir, each
// wrapped in its own AsyncJobStore (its own host identity once it claims),
// with every named session pre-seeded.
func nStores(t *testing.T, n int, sessionIDs ...string) (stores []*AsyncJobStore, q *db.Queries, ctx context.Context) {
	t.Helper()
	dataDir := t.TempDir()
	ctx = context.Background()
	setup, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	for _, id := range sessionIDs {
		require.NoError(t, seedSession(ctx, db.New(setup), id))
	}
	require.NoError(t, db.Release(dataDir))

	path := dataDir + "/rush.db"
	for i := 0; i < n; i++ {
		conn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
		require.NoError(t, err)
		conn.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = conn.Close() })
		st := NewAsyncJobStore(conn, dataDir, 1000+i, "store")
		t.Cleanup(func() { _ = st.Close(context.Background()) })
		stores = append(stores, st)
	}
	return stores, stores[0].q, ctx
}

func driverRow(t *testing.T, ctx context.Context, st *AsyncJobStore, sessionID string) (db.SessionDriver, bool) {
	t.Helper()
	row, err := st.q.GetSessionDriver(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return db.SessionDriver{}, false
	}
	require.NoError(t, err)
	return row, true
}

// TestClaimSessionDriver_IdempotentReleaseAndClose: a claim is idempotent for
// the same host, only the owner's Release deletes it, and Close (a clean
// process exit) deletes every marker of its host.
//
// Revert-check: (a) drop the DeleteSessionDriversForHost call from Close; (b)
// drop the host scope (`AND host_id = ?`) from DeleteSessionDriver.
func TestClaimSessionDriver_IdempotentReleaseAndClose(t *testing.T) {
	t.Parallel()
	stores, _, ctx := nStores(t, 2, "s1", "s2", "s3")
	a, b := stores[0], stores[1]

	require.NoError(t, a.ClaimSessionDriver(ctx, "s1"))
	require.NoError(t, a.ClaimSessionDriver(ctx, "s1"), "a repeated claim by the same host is a no-op")
	row, ok := driverRow(t, ctx, a, "s1")
	require.True(t, ok)
	require.Equal(t, a.HostID(), row.HostID)
	require.EqualValues(t, 1000, row.Pid)

	// Another host's release never deletes someone else's claim (b has a real
	// host of its own here, so this is not the no-host no-op).
	require.NoError(t, b.ClaimSessionDriver(ctx, "s3"))
	require.NoError(t, b.ReleaseSessionDriver(ctx, "s1"))
	_, ok = driverRow(t, ctx, a, "s1")
	require.True(t, ok, "only the owning host may release its marker")

	require.NoError(t, a.ReleaseSessionDriver(ctx, "s1"))
	_, ok = driverRow(t, ctx, a, "s1")
	require.False(t, ok)

	// Close deletes every marker of the closing host.
	require.NoError(t, a.ClaimSessionDriver(ctx, "s1"))
	require.NoError(t, a.ClaimSessionDriver(ctx, "s2"))
	require.NoError(t, a.Close(ctx))
	_, ok = driverRow(t, ctx, b, "s1")
	require.False(t, ok, "Close must delete the host's markers")
	_, ok = driverRow(t, ctx, b, "s2")
	require.False(t, ok)
}

// TestClaimSessionDriver_LiveRefused_DeadTakenOver: a live (sibling) host's
// marker blocks a second claimant with a typed error naming the host and pid;
// once that host is provably dead the second claimant takes the marker over.
//
// Revert-check: skip the liveness check (treat every foreign marker as
// takeable) -- B's first claim succeeds while A is alive.
func TestClaimSessionDriver_LiveRefused_DeadTakenOver(t *testing.T) {
	t.Parallel()
	stores, _, ctx := nStores(t, 2, "s1")
	a, b := stores[0], stores[1]
	require.NoError(t, a.ClaimSessionDriver(ctx, "s1"))

	err := b.ClaimSessionDriver(ctx, "s1")
	var elsewhere *ErrSessionDrivenElsewhere
	require.ErrorAs(t, err, &elsewhere, "a live driver must refuse a second claimant")
	require.Equal(t, a.HostID(), elsewhere.HostID)
	require.EqualValues(t, 1000, elsewhere.PID)
	require.Equal(t, HostStatusAlive, elsewhere.Status)
	require.Contains(t, err.Error(), "pid 1000")
	row, _ := driverRow(t, ctx, b, "s1")
	require.Equal(t, a.HostID(), row.HostID, "a refused claim must not change the marker")

	deadHost := a.HostID()
	require.NoError(t, a.SimulateCrashForTest())
	require.NoError(t, b.ClaimSessionDriver(ctx, "s1"), "a dead host's marker is taken over")
	row, ok := driverRow(t, ctx, b, "s1")
	require.True(t, ok)
	require.Equal(t, b.HostID(), row.HostID)
	require.NotEqual(t, deadHost, row.HostID)
	require.EqualValues(t, 1001, row.Pid)
}

// TestClaimSessionDriver_ConcurrentTakeoverOneWinner: eight stores race to
// take over a dead host's marker; exactly one succeeds, the rest are refused
// by the new (live) owner.
//
// Revert-check: make TakeOverSessionDriver unconditional (drop the
// `host_id = expected_host_id` predicate) -- several claims return nil.
func TestClaimSessionDriver_ConcurrentTakeoverOneWinner(t *testing.T) {
	// Not parallel: it owns the package-level seam.
	const racers = 8
	stores, _, ctx := nStores(t, racers+1, "s1")
	dead := stores[0]
	require.NoError(t, dead.ClaimSessionDriver(ctx, "s1"))
	require.NoError(t, dead.SimulateCrashForTest())

	// Hold every racer after it read the dead row and before it writes, so all
	// of them really contend on the takeover CAS (without the seam the racers
	// are serialized by their own host registration and never overlap).
	var arrived atomic.Int32
	allRead := make(chan struct{})
	sessionDriverBeforeTakeoverSeam = func() {
		if arrived.Add(1) == racers {
			close(allRead)
		}
		select {
		case <-allRead:
		case <-time.After(10 * time.Second):
		}
	}
	t.Cleanup(func() { sessionDriverBeforeTakeoverSeam = nil })

	var wins atomic.Int32
	var refused atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, st := range stores[1:] {
		wg.Add(1)
		go func(st *AsyncJobStore) {
			defer wg.Done()
			<-start
			err := st.ClaimSessionDriver(ctx, "s1")
			var elsewhere *ErrSessionDrivenElsewhere
			switch {
			case err == nil:
				wins.Add(1)
			case errors.As(err, &elsewhere):
				refused.Add(1)
			default:
				t.Errorf("unexpected claim error: %v", err)
			}
		}(st)
	}
	close(start)
	wg.Wait()

	require.EqualValues(t, 1, wins.Load(), "exactly one takeover may win")
	require.EqualValues(t, racers-1, refused.Load(), "every loser is refused by the new owner")
	row, ok := driverRow(t, ctx, dead, "s1")
	require.True(t, ok)
	winners := 0
	for _, st := range stores[1:] {
		if st.HostID() == row.HostID {
			winners++
		}
	}
	require.Equal(t, 1, winners, "the marker names exactly the winning host")
}

// TestClaimSessionDriver_MarkerUnavailableWithoutHostLock: when the host lock
// cannot be registered (no OS-lock-capable hosts dir) the claim reports
// ErrDriverMarkerUnavailable so the caller can fall back to its in-memory
// marker.
func TestClaimSessionDriver_MarkerUnavailableWithoutHostLock(t *testing.T) {
	t.Parallel()
	stores, _, ctx := nStores(t, 1, "s1")
	st := stores[0]
	// A FILE where the hosts directory must be: no lock file can be created.
	require.NoError(t, os.WriteFile(HostsDir(st.dataDir), []byte("x"), 0o644))

	err := st.ClaimSessionDriver(ctx, "s1")
	require.ErrorIs(t, err, ErrDriverMarkerUnavailable)
	_, ok := driverRow(t, ctx, st, "s1")
	require.False(t, ok, "no marker may be written without a host to name")
}

// registerForeignLiveHost registers a real host (its lock file is held by this
// test) and drops it from the process-wide own-id registry, so every probe of
// it takes the cross-process path -- the shared probe against a genuinely held
// lock -- exactly as another process's host would. Same-process sibling stores
// (nStores) never reach that branch: IsOwnHostID answers "alive" for them.
func registerForeignLiveHost(t *testing.T, ctx context.Context, q *db.Queries, dataDir string) string {
	t.Helper()
	h, err := RegisterHost(ctx, dataDir, 4321, "foreign", q)
	require.NoError(t, err)
	unmarkOwnHostID(h.ID)
	t.Cleanup(func() { _ = h.Close(ctx, q) })
	return h.ID
}

// TestForeignLiveDriver_Matrix: none / own / live sibling / genuinely foreign
// live / crashed / unknown liveness -> false / false / true / true / false /
// true (unknown counts as alive). The "live sibling" case is a same-process
// store answered by IsOwnHostID without probing; the "foreign live" case is the
// one that reaches the shared probe with a really held lock.
//
// Revert-check: (a) treat unknown as dead; (b) drop the own-host check --
// the own case reports foreign (the sibling registry says its own host is
// alive); (c) make hostLivenessDetail fold a contended (alive) probe into dead
// -- the foreign-live case reports not foreign.
func TestForeignLiveDriver_Matrix(t *testing.T) {
	t.Parallel()
	stores, q, ctx := nStores(t, 3, "none", "own", "sibling", "foreign-live", "crashed", "unknown")
	a, b, c := stores[0], stores[1], stores[2]

	check := func(st *AsyncJobStore, sessionID string, wantForeign bool, why string) {
		t.Helper()
		_, foreign, err := st.ForeignLiveDriver(ctx, sessionID)
		require.NoError(t, err)
		require.Equal(t, wantForeign, foreign, why)
	}

	check(b, "none", false, "no marker -> not foreign")

	require.NoError(t, b.ClaimSessionDriver(ctx, "own"))
	check(b, "own", false, "a marker naming this store's own host is not foreign")

	require.NoError(t, a.ClaimSessionDriver(ctx, "sibling"))
	d, foreign, err := b.ForeignLiveDriver(ctx, "sibling")
	require.NoError(t, err)
	require.True(t, foreign, "a live sibling driver is foreign")
	require.Equal(t, a.HostID(), d.HostID)
	require.Equal(t, HostStatusAlive, d.Status)
	require.EqualValues(t, 1000, d.PID)

	foreignID := registerForeignLiveHost(t, ctx, q, b.dataDir)
	n, err := q.InsertSessionDriver(ctx, db.InsertSessionDriverParams{SessionID: "foreign-live", HostID: foreignID, Pid: 4321, ClaimedAt: 1})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.False(t, IsOwnHostID(foreignID), "precondition: the own-id shortcut must not answer for this host")
	d, foreign, err = b.ForeignLiveDriver(ctx, "foreign-live")
	require.NoError(t, err)
	require.True(t, foreign, "a driver whose host holds its lock is foreign and live")
	require.Equal(t, HostStatusAlive, d.Status, "the verdict must come from the shared probe of a really held lock")

	require.NoError(t, c.ClaimSessionDriver(ctx, "crashed"))
	check(b, "crashed", true, "precondition: alive before the crash")
	require.NoError(t, c.SimulateCrashForTest())
	check(b, "crashed", false, "a provably dead driver is not foreign")

	// Unknown: the lock path is a directory, so the probe cannot decide.
	require.NoError(t, os.MkdirAll(HostLockPath(b.dataDir, "unknown-host"), 0o755))
	n, err = q.InsertSessionDriver(ctx, db.InsertSessionDriverParams{SessionID: "unknown", HostID: "unknown-host", Pid: 4242, ClaimedAt: 1})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	d, foreign, err = b.ForeignLiveDriver(ctx, "unknown")
	require.NoError(t, err)
	require.True(t, foreign, "unknown liveness counts as alive")
	require.Equal(t, HostStatusUnknown, d.Status)
}

// TestPurgeExpired_DeletesOnlyDeadDriverRows: the retention pass drops the
// markers of provably dead hosts and leaves live, unknown and its own alone.
// "live" is a same-process sibling (skipped by IsOwnHostID before any probe);
// "foreign-live" is a genuinely foreign host whose lock is really held, so the
// pass must reach the shared probe and read "alive" from it.
//
// Revert-check: (a) delete markers without probing (every foreign host's row
// goes); (b) treat a contended (alive) probe as dead -- the foreign-live marker
// is purged and its survival assertion fails.
func TestPurgeExpired_DeletesOnlyDeadDriverRows(t *testing.T) {
	t.Parallel()
	stores, q, ctx := nStores(t, 3, "live", "foreign-live", "dead", "unknown", "mine")
	purger, live, dead := stores[0], stores[1], stores[2]

	require.NoError(t, live.ClaimSessionDriver(ctx, "live"))
	require.NoError(t, dead.ClaimSessionDriver(ctx, "dead"))
	require.NoError(t, purger.ClaimSessionDriver(ctx, "mine"))
	foreignID := registerForeignLiveHost(t, ctx, q, purger.dataDir)
	_, err := q.InsertSessionDriver(ctx, db.InsertSessionDriverParams{SessionID: "foreign-live", HostID: foreignID, Pid: 4321, ClaimedAt: 1})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(HostLockPath(purger.dataDir, "unknown-host"), 0o755))
	_, err = q.InsertSessionDriver(ctx, db.InsertSessionDriverParams{SessionID: "unknown", HostID: "unknown-host", Pid: 1, ClaimedAt: 1})
	require.NoError(t, err)
	require.NoError(t, dead.SimulateCrashForTest())

	require.NoError(t, purger.PurgeExpired(ctx, 7*24*time.Hour))

	_, ok := driverRow(t, ctx, purger, "dead")
	require.False(t, ok, "a dead host's marker must be purged")
	for _, id := range []string{"live", "foreign-live", "unknown", "mine"} {
		_, ok := driverRow(t, ctx, purger, id)
		require.True(t, ok, "marker %s must survive the purge", id)
	}
}

// TestSessionDrivers_CascadeOnSessionDelete: deleting a session takes its
// marker with it (FK ON DELETE CASCADE).
func TestSessionDrivers_CascadeOnSessionDelete(t *testing.T) {
	t.Parallel()
	stores, q, ctx := nStores(t, 1, "s1")
	require.NoError(t, stores[0].ClaimSessionDriver(ctx, "s1"))
	_, ok := driverRow(t, ctx, stores[0], "s1")
	require.True(t, ok)

	require.NoError(t, q.DeleteSession(ctx, "s1"))
	_, ok = driverRow(t, ctx, stores[0], "s1")
	require.False(t, ok)
}
