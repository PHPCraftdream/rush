// HostStore coverage (package R12-D chunk 1): the retained host lifecycle
// exercised against a database without async_jobs (H4 final world: the table
// no longer exists; the helper's conditional drop keeps the dropped-table
// hardening meaningful on any base that still creates it). Any accidental
// async_jobs query in the retained paths errors ("no such table:
// async_jobs"), which is what makes these tests real: they prove the
// lifecycle is job-table-independent. batch_nodes is used to drive the
// DeleteAsyncHostIfNoNodes guard.
package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// newHostTestDB opens a real, migrated SQLite DB via db.Connect in a fresh
// temp dir, then drops async_jobs when it still exists. Connect itself only
// runs migrations (goose Up) and opens the pool -- nothing after Connect in
// these tests reads async_jobs, so the drop is safe immediately after it
// returns and turns any hidden dependency into a loud "no such table" error.
func newHostTestDB(t *testing.T) (*sql.DB, *db.Queries, string, context.Context) {
	t.Helper()
	dataDir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	_, err = conn.ExecContext(ctx, "DROP TABLE IF EXISTS async_jobs")
	require.NoError(t, err, "the async_jobs drop must always succeed")
	return conn, db.New(conn), dataDir, ctx
}

// newHostTestStore builds a HostStore over newHostTestDB's connection.
func newHostTestStore(t *testing.T) (*HostStore, *sql.DB, *db.Queries, string, context.Context) {
	t.Helper()
	conn, q, dataDir, ctx := newHostTestDB(t)
	store := NewHostStore(conn, dataDir, 999, "test")
	// Release the host's OS lock file (if still held) before t.TempDir()'s
	// cleanup -- Windows refuses to delete a file this process holds open.
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store, conn, q, dataDir, ctx
}

// TestHostStore_EnsureHostMemoizedWithoutAsyncJobs proves register/ensure
// works with async_jobs gone: lazy, memoized (same id both calls), the row
// is readable via the new host queries, and HostID is empty before the
// first EnsureHost.
func TestHostStore_EnsureHostMemoizedWithoutAsyncJobs(t *testing.T) {
	t.Parallel()
	store, _, q, _, ctx := newHostTestStore(t)

	require.Empty(t, store.HostID(), "HostID must be empty before the first EnsureHost")

	id1, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, id1)
	require.Equal(t, id1, store.HostID())

	id2, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	require.Equal(t, id1, id2, "EnsureHost must be memoized: same host id on every call")

	// Exactly ONE async_hosts row, readable via the NEW job-independent query.
	row, err := q.GetAsyncHostRow(ctx, id1)
	require.NoError(t, err)
	require.Equal(t, id1, row.ID)
	require.EqualValues(t, 999, row.Pid)
	require.Equal(t, "test", row.Label)

	hosts, err := q.ListAsyncHostRows(ctx)
	require.NoError(t, err)
	require.Len(t, hosts, 1)

	// The host owns no batch_nodes rows, so it is a no-nodes candidate.
	noNodes, err := q.ListAsyncHostsWithNoNodes(ctx)
	require.NoError(t, err)
	require.Len(t, noNodes, 1)
	require.Equal(t, id1, noNodes[0].ID)
}

// TestHostStore_CloseDeletesHostRowWhenNoNodes proves the clean exit: with
// zero batch_nodes rows the host row AND the lock file are both removed
// (HostIdentity.Close's "rows > 0 -> remove file" arm, driven by the
// nodes-based guard).
func TestHostStore_CloseDeletesHostRowWhenNoNodes(t *testing.T) {
	t.Parallel()
	store, _, q, dataDir, ctx := newHostTestStore(t)

	id, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dataDir, "hosts", id+".lock"))

	require.NoError(t, store.Close(ctx))

	_, err = q.GetAsyncHostRow(ctx, id)
	require.ErrorIs(t, err, sql.ErrNoRows, "the host row must be deleted when no batch_nodes rows exist")
	require.NoFileExists(t, filepath.Join(dataDir, "hosts", id+".lock"), "the lock file must be removed on a clean exit")

	// HostID is forgotten after Close.
	require.Empty(t, store.HostID())
}

// TestHostStore_CloseKeepsHostRowWhenNodesExist proves the guard's keep arm
// AND the DeleteAsyncHostIfNoNodes live-set semantics directly: a host owning
// a LIVE batch_nodes row survives Close; a TERMINAL ('completed') row does
// NOT keep the host row (deliberate R12-D semantic change, documented in
// async_hosts.sql); once the live row is deleted, the SAME nodes-based query
// deletes the host row.
func TestHostStore_CloseKeepsHostRowWhenNodesExist(t *testing.T) {
	t.Parallel()
	store, conn, q, _, ctx := newHostTestStore(t)

	id, err := store.EnsureHost(ctx)
	require.NoError(t, err)

	require.NoError(t, seedSession(ctx, q, "owner-bn"))
	_, err = conn.ExecContext(ctx, `
		INSERT INTO batch_nodes (
			node_id, root_id, owner_session_id, host_id, ordinal, kind, mode,
			tool_call_id, input_hash, state, created_at, updated_at
		) VALUES ('root-1', 'root-1', 'owner-bn', ?, 0, 'batch', 'parallel',
		          'call-bn-1', 'hash-bn-1', 'completed', 1728600000, 1728600000)`,
		id)
	require.NoError(t, err)

	// A terminal node alone must NOT retain the host row.
	require.NoError(t, store.Close(ctx))
	_, err = q.GetAsyncHostRow(ctx, id)
	require.ErrorIs(t, err, sql.ErrNoRows,
		"terminal node history alone must NOT keep the host row (live-set rule)")

	// A live node MUST keep the host row: register a fresh host and retry.
	store2 := NewHostStore(conn, t.TempDir(), 998, "test-live")
	t.Cleanup(func() { _ = store2.Close(context.Background()) })
	id2, err := store2.EnsureHost(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
		INSERT INTO batch_nodes (
			node_id, root_id, owner_session_id, host_id, ordinal, kind, mode,
			tool_call_id, input_hash, state, created_at, updated_at
		) VALUES ('root-2', 'root-2', 'owner-bn', ?, 0, 'batch', 'parallel',
		          'call-bn-2', 'hash-bn-2', 'running', 1728600001, 1728600001)`,
		id2)
	require.NoError(t, err)

	require.NoError(t, store2.Close(ctx))
	row, err := q.GetAsyncHostRow(ctx, id2)
	require.NoError(t, err, "a LIVE batch_nodes row must keep the host row")
	require.Equal(t, id2, row.ID)

	// ...and the host is NOT a no-live-nodes retention candidate anymore.
	noNodes, err := q.ListAsyncHostsWithNoNodes(ctx)
	require.NoError(t, err)
	require.Empty(t, noNodes,
		"a host with a live batch_nodes row must not be a retention candidate")

	// Deleting the live node row releases the host row to the SAME guard.
	res, err := conn.ExecContext(ctx, `DELETE FROM batch_nodes WHERE root_id = 'root-2'`)
	require.NoError(t, err)
	deleted, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)

	rows, err := q.DeleteAsyncHostIfNoNodes(ctx, id2)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows, "with the last LIVE batch_nodes row gone, the host row must be deletable")

	_, err = q.GetAsyncHostRow(ctx, id2)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestHostStore_CloseKeepLockKeepsRowAndFile proves the forced-shutdown
// path (contract IDENTICAL to AsyncJobStore.CloseKeepLock, A12): the
// in-memory host handle is forgotten -- HostID() returns "" afterwards,
// exactly like the legacy CloseKeepLock -- but the async_hosts row and the
// lock file survive, the own-host-id marking survives, and the OS lock stays
// HELD until process exit (proven by the kernel via a second SHARED probe:
// a shared lock can only be taken when no exclusive holder exists).
//
// NOT t.Parallel(): this test's cleanup calls releaseRetainedHostLocksForTest
// (host_lock.go:112-118), which releases the PROCESS-GLOBAL retained-lock
// registry -- the unchanged parallel legacy pin test
// (async_job_recovery_test.go, TestAsyncJobStore_CloseKeepLock_LeavesLockHeld)
// also depends on that registry holding ITS lock past GC. Running the two
// cleanups concurrently could release the other test's lock before its
// finalization assertion, so this test stays serial.
func TestHostStore_CloseKeepLockKeepsRowAndFile(t *testing.T) {
	store, _, q, dataDir, ctx := newHostTestStore(t)

	id, err := store.EnsureHost(ctx)
	require.NoError(t, err)

	store.CloseKeepLock()

	// Cleanup BEFORE any assertion: the lock was pinned until process exit;
	// release it and drop the own-id marking so t.TempDir()'s cleanup can
	// delete the directory (Windows refuses to delete open files).
	t.Cleanup(func() {
		releaseRetainedHostLocksForTest()
		unmarkOwnHostID(id)
	})

	require.Empty(t, store.HostID(),
		"CloseKeepLock takes the host handle: HostID is empty afterwards (same as AsyncJobStore.CloseKeepLock)")
	_, err = q.GetAsyncHostRow(ctx, id)
	require.NoError(t, err, "the async_hosts row must survive CloseKeepLock")
	lockPath := HostLockPath(dataDir, id)
	require.FileExists(t, lockPath)
	require.True(t, IsOwnHostID(id), "the own-host-id marking must survive CloseKeepLock")

	// Kernel-attested proof the lock is actually still held. No test-side
	// reference to the lock is kept: without the package-level pin the
	// *os.File finalizer releases it at GC. Force finalization like the
	// legacy pin test does (async_job_recovery_test.go,
	// TestAsyncJobStore_CloseKeepLock_LeavesLockHeld) BEFORE probing, so a
	// dropped pin fails here too. REVERT CHECK: drop retainHostLockUntilExit
	// from CloseKeepLock -> the finalizer closes the unreferenced file and
	// this probe reads Dead. A SHARED probe only succeeds when NO exclusive
	// holder exists, so Alive means someone (this store, pinned until exit)
	// holds the exclusive lock.
	for range 3 {
		runtime.GC()
		time.Sleep(30 * time.Millisecond)
	}
	status, err := ProbeHostLockShared(lockPath)
	require.NoError(t, err)
	require.Equal(t, HostStatusAlive, status, "the OS lock must stay pinned until process exit")
}

// TestHostStore_SimulateCrashForTest models a dead process: the OS lock is
// released and the own-id marking forgotten, but the async_hosts row and
// lock file stay on disk -- a later ProbeHostShared from this process must
// see the host as dead, with NO async_jobs involved anywhere.
func TestHostStore_SimulateCrashForTest(t *testing.T) {
	t.Parallel()
	store, _, q, dataDir, ctx := newHostTestStore(t)

	id, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	require.NoError(t, store.SimulateCrashForTest())

	require.False(t, IsOwnHostID(id))
	_, err = q.GetAsyncHostRow(ctx, id)
	require.NoError(t, err, "the async_hosts row must survive a simulated crash")

	status, err := ProbeHostShared(dataDir, id)
	require.NoError(t, err)
	require.Equal(t, HostStatusDead, status, "with the lock released, another process would see this host as dead")
}
