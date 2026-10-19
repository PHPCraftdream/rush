// R12-F chunk C tests: the flag-ON (RUSH_BATCH_RUNTIME) host identity and
// teardown are independent of the async_jobs table (session.HostStore), the
// shutdown drain completes, a REAL cross-process locked-host fault recovers,
// and the flag-ON readers (drivers, wait heartbeat) use batch_nodes plus the
// shared lock probe. Flag-off equivalence is pinned by the untouched
// AsyncJobStore branches (TestBatchRuntimeFlagOffGolden and the flag-off
// readers' existing tests).

package app

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestBatchHostStoreFlagOn_TeardownWithoutAsyncJobsTable (R12-F acceptance
// 4 of the decomposition): a REAL app built through New with the flag ON
// against a real SQLite data dir, then DROP TABLE async_jobs, then the real
// teardown through ShutdownWithResult must end with no cleanup errors: the
// flag-ON Close path releases the host identity through the HostStore's
// DeleteAsyncHostIfNoNodes and never queries async_jobs.
//
// Revert-check: pointing the flag-ON install/telemetry back at the
// AsyncJobStore makes EnsureHost fail at New (install) or the teardown's
// DeleteAsyncHostIfNoJobs fail after the drop — either surfaces as a
// non-empty CleanupErrors or a New error.
func TestBatchHostStoreFlagOn_TeardownWithoutAsyncJobsTable(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	app := newFullNewApp(t)
	require.NotNil(t, app.batchRuntime, "flag on: runtime installed")
	hostID := app.BatchHostStore().HostID()
	require.NotEmpty(t, hostID)
	// First real-schema assertion: the HostStore wrote the async_hosts row
	// (the flag-ON identity's durable bookkeeping) and wrote NOTHING to the
	// AsyncJobStore identity.
	var hosts int
	require.NoError(t, app.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM async_hosts WHERE id = ?`, hostID).Scan(&hosts))
	require.Equal(t, 1, hosts, "flag on: the HostStore registered async_hosts row")
	require.Empty(t, app.asyncJobStore.HostID(), "flag on: async_jobs identity never ensured")

	// The cutover end-state: the table is GONE; only the flag-ON branch can
	// survive it.
	_, err := app.DB().ExecContext(context.Background(), `DROP TABLE async_jobs`)
	require.NoError(t, err)

	res := app.ShutdownWithResult()
	require.False(t, res.Forced)
	require.Empty(t, res.CleanupErrors,
		"the flag-ON teardown never queries async_jobs")

	// All-OS leak oracle: after the real flag-ON shutdown, EVERY lock file
	// under hosts/ must be exclusively re-acquirable. Windows fails the
	// acquire on an open handle (sharing violation); POSIX flock fails it on
	// a held lock — unlike os.Remove, which succeeds on POSIX even for an
	// open, flocked file. A file already unlinked by the graceful Close is
	// simply absent from the glob. Release each acquired lock so later
	// cleanups see no new handles.
	entries, rerr := os.ReadDir(session.HostsDir(app.dataDir))
	require.NoError(t, rerr)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".lock" {
			continue
		}
		lk, aerr := filelock.TryAcquireFileLock(session.HostLockPath(app.dataDir, strings.TrimSuffix(e.Name(), ".lock")))
		require.NoError(t, aerr, "hosts/%s must be re-acquirable after shutdown (a failure means a host lock handle leaked)", e.Name())
		if lk != nil {
			require.NoError(t, lk.Release())
		}
	}
}

// TestBatchHostStoreFlagOn_ShutdownDrainCompletes (R12-F acceptance: the
// shutdown drain completes flag ON): the real gate (DrainAndRelease) runs
// through ShutdownWithResult on a flag-ON app and the HostStore release
// finishes gracefully — no forced verdict, no cleanup errors.
//
// Revert-check: wiring RetainHostIdentity/ReleaseHostIdentity back to the
// AsyncJobStore keeps this green only while async_jobs exists; combined with
// the table-drop test above it pins the HostStore wiring, and reverting the
// gate itself (nil release seams) fails the Forced/CleanupErrors asserts.
func TestBatchHostStoreFlagOn_ShutdownDrainCompletes(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	app := newFullNewApp(t)
	hostID := app.BatchHostStore().HostID()
	require.NotEmpty(t, hostID)
	var hosts int
	require.NoError(t, app.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM async_hosts WHERE id = ?`, hostID).Scan(&hosts))
	require.Equal(t, 1, hosts, "first real-schema assertion: HostStore registered the host")

	res := app.ShutdownWithResult()
	require.False(t, res.Forced, "dormant runtime: the drain completes gracefully")
	require.Empty(t, res.CleanupErrors)
}

// TestBatchHostShutdownReleasesEveryHostFlagOn (R12-F): a flag-ON app whose
// LEGACY AsyncJobStore host was ensured directly (the pre-R12-F fixture
// shape; production flag-ON code ensures only the HostStore) releases BOTH
// hosts on the real ShutdownWithResult path — the drain gate releases the
// HostStore, and releaseResources' runtime branch releases the legacy host
// BEFORE the gate's writer release.
//
// Revert-check: dropping the legacy-release block from releaseResources'
// runtime branch leaves the legacy hosts/<id>.lock held; the acquire below
// fails with a sharing violation on Windows and a held flock on POSIX.
func TestBatchHostShutdownReleasesEveryHostFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	app := newFullNewApp(t)
	require.NotNil(t, app.batchRuntime, "flag on: runtime installed")
	_, err := app.asyncJobStore.EnsureHost(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, app.BatchHostStore().HostID(), "the install registered the HostStore identity too")

	res := app.ShutdownWithResult()
	require.False(t, res.Forced)
	require.Empty(t, res.CleanupErrors)

	// Graceful Close deletes released lock files, so an empty dir is a pass;
	// a leaked lock stays listed and fails the acquire below.
	entries, rerr := os.ReadDir(session.HostsDir(app.dataDir))
	if !os.IsNotExist(rerr) {
		require.NoError(t, rerr)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".lock" {
			continue
		}
		lk, aerr := filelock.TryAcquireFileLock(session.HostLockPath(app.dataDir, strings.TrimSuffix(e.Name(), ".lock")))
		require.NoError(t, aerr, "hosts/%s must be re-acquirable after shutdown", e.Name())
		if lk != nil {
			require.NoError(t, lk.Release())
		}
	}
}

// TestBatchHostLockSubprocessHelper is the guarded helper process: it
// registers a REAL host (OS lock on <dataDir>/hosts/<id>.lock plus the
// async_hosts row) against the data dir named by the environment and blocks
// forever holding the lock. It only ever runs when the test binary is
// re-invoked with R12F_HOST_LOCK_HELPER=1 (see the parent below).
func TestBatchHostLockSubprocessHelper(t *testing.T) {
	if os.Getenv("R12F_HOST_LOCK_HELPER") != "1" {
		t.Skip("helper process only")
	}
	dataDir := os.Getenv("R12F_HELPER_DATA_DIR")
	idFile := os.Getenv("R12F_HELPER_HOST_ID_FILE")
	if dataDir == "" || idFile == "" {
		os.Exit(2)
	}
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	if err != nil {
		os.Exit(3)
	}
	// Release through the pool accounting (db.Release), not conn.Close: the
	// pool is ref-counted per data dir and os.Exit skips defers, so every
	// exit path below releases explicitly before exiting.
	defer func() { _ = db.Release(dataDir) }()
	h, err := session.RegisterHost(ctx, dataDir, os.Getpid(), "r12f-helper",
		session.AsyncHostStoreFromConn(conn))
	if err != nil {
		_ = db.Release(dataDir)
		os.Exit(4)
	}
	// Pin the identity for the helper's whole life: after the WriteFile below
	// nothing else references h, and a GC cycle would finalize the lock's
	// *os.File, closing the fd and silently dropping the OS lock (the parent
	// then probes the host DEAD instead of ALIVE).
	defer runtime.KeepAlive(h)
	if err := os.WriteFile(idFile, []byte(h.ID), 0o644); err != nil {
		_ = db.Release(dataDir)
		os.Exit(5)
	}
	select {} // hold the OS host lock until the parent kills us
}

// TestBatchHostStoreFlagOn_LockedHostFaultThenRecovery (R12-F acceptance:
// "real locked-host fault subprocess plus subsequent recovery"): a real
// subprocess holds a real host lock in a shared data dir; the parent probes
// it ALIVE through the shared lock probe (a REAL lock the parent cannot
// take), kills the holder (the crash model — the OS lock dies with the
// process), probes it DEAD, and then a REAL flag-ON app built over the same
// data dir runs the production dead-host recovery against the persisted work
// before shutting down cleanly.
//
// Coverage: real batch work (a running root plus a running task child, seeded
// through the real BatchStore reservation path before the crash) is settled
// by the production dead-host recovery entry BatchRecoverDeadHost: the
// unannounced root's whole subtree is deleted root-atomically
// (BatchRecoveryOutcome.DeletedRoots == 1), an ANNOUNCED root with live nodes
// takes the terminal-interruption arm (state = 'interrupted',
// outcome_code = 'host_lost', Interrupted == 1, its root notice selected),
// and a subsequent BatchRequestStop on the deleted root returns
// session.ErrBatchControlStale (the dead-host dispatch path, which wires to
// recovery) without further transitions; a second recovery is a no-op (no
// replay).
//
// Revert-check: removing the HostStatusDead -> BatchRecoverDeadHost branch at
// internal/session/batch_control.go (the dead-host recovery dispatch) leaves
// the seeded roots unsettled and fails the subtree-deleted and 'interrupted'
// asserts; removing batchRecoverRoot's settlement (DeleteBatchSubtree) fails
// the DeletedRoots count and removing the interrupt arm fails the
// Interrupted count. Reverting the HostStore probe wiring (e.g. liveness
// through a store that never sees the foreign lock) fails the Alive/Dead
// asserts.
func TestBatchHostStoreFlagOn_LockedHostFaultThenRecovery(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	dataDir := t.TempDir()
	idFile := filepath.Join(t.TempDir(), "host_id")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := platform.Command(ctx, exe, "-test.run=^TestBatchHostLockSubprocessHelper$")
	cmd.Env = append(os.Environ(),
		"R12F_HOST_LOCK_HELPER=1",
		"R12F_HELPER_DATA_DIR="+dataDir,
		"R12F_HELPER_HOST_ID_FILE="+idFile,
	)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	// Wait for the helper's REAL registration (it writes the id only after
	// RegisterHost took the OS lock).
	var hostID string
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, rerr := os.ReadFile(idFile)
		if rerr == nil && len(b) > 0 {
			hostID = strings.TrimSpace(string(b))
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper never registered its host lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.NotEmpty(t, hostID)

	// Seed REAL persisted batch work owned by the helper host through the
	// REAL store reservation path, via a throwaway handle on the shared data
	// dir (same Connect/Release pattern as the helper process above).
	seedConn, serr := db.Connect(ctx, dataDir)
	require.NoError(t, serr)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	// Two-result call: CreateSession returns (Session, error) — see
	// internal/db/sessions.sql.go createSession (:one) and the seedSession
	// helper in internal/session/host_lock_test.go, which passes only ID and
	// Title (every other listed column keeps its INSERT zero/default value;
	// none of them violates a sessions CHECK at zero).
	seedQueries := db.New(seedConn)
	_, cerr := seedQueries.CreateSession(ctx, db.CreateSessionParams{
		ID: "r12f-parent", Title: "r12f-parent",
	})
	require.NoError(t, cerr)
	bs := session.NewBatchStore(seedConn)
	reserved, serr := bs.Reserve(ctx, session.BatchReservation{
		Owner: "r12f-parent", Host: hostID,
		ToolCallID: "call-r12f-recovery", InputHash: "hash-r12f-recovery",
		Sequence: 1,
		Root: session.BatchReservationNode{
			Values: db.ReserveBatchNodeParams{
				Kind: "batch",
				Mode: nullString("parallel"), State: "running", ControlState: "none",
			},
			Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
				Kind: "task", TaskKind: nullString("test"), State: "running",
				ControlState: "none",
			}}},
		},
	})
	require.NoError(t, serr)
	rootID := reserved.Snapshot.Root.RootID
	require.NotEmpty(t, rootID)
	// The work exists, live, BEFORE the crash gate.
	var seedState string
	require.NoError(t, seedConn.QueryRowContext(ctx,
		`SELECT state FROM batch_nodes WHERE root_id = ? AND node_id = root_id`, rootID).Scan(&seedState))
	require.Equal(t, "running", seedState, "seeded root is live before the crash")

	// The locked-host fault, probed for real: the parent cannot win the
	// lock while the helper lives.
	st, err := session.ProbeHostShared(dataDir, hostID)
	require.NoError(t, err)
	require.Equal(t, session.HostStatusAlive, st, "helper holds the real host lock")

	// Crash: kill -9 equivalent. The OS lock dies with the process.
	require.NoError(t, cmd.Process.Kill())
	_, _ = cmd.Process.Wait()
	st, err = session.ProbeHostShared(dataDir, hostID)
	require.NoError(t, err)
	require.Equal(t, session.HostStatusDead, st, "crashed host is provably dead")

	// Recovery with the flag ON: a real app over the same data dir installs
	// its own HostStore identity (a fresh host, the crashed one unrecoverable
	// and irrelevant) and tears down cleanly through the real gate.
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	app := newFullNewAppAt(t, dataDir)
	got := app.BatchHostStore().HostID()
	require.NotEmpty(t, got)
	require.NotEqual(t, hostID, got, "recovered app runs on its own host identity")
	var hosts int
	require.NoError(t, app.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM async_hosts WHERE id = ?`, got).Scan(&hosts))
	require.Equal(t, 1, hosts, "first real-schema assertion: recovered HostStore row")

	// Production recovery entry: BatchRecoverDeadHost (internal/session/
	// batch_recovery.go:25) against the crashed host. Two roots are listed by
	// BatchRecoveryRoots (batch_recovery.sql:3-9):
	//   - the unannounced root (announced=0 matches the first disjunct, line 5;
	//     the seeded root keeps the schema's announced default) takes
	//     batchRecoverRoot's DeleteBatchSubtree arm (batch_recovery.go:74-75):
	//     DeletedRoots == 1;
	//   - the ANNOUNCED root seeded below (announced=1, live 'running' nodes)
	//     takes the terminal-interruption arm (batch_recovery.go:77+ via
	//     BatchRecoveryInterrupt, batch_recovery.sql:18-26: state='interrupted',
	//     outcome_code='host_lost'): Interrupted == 1, and its root notice is
	//     selected by BatchRecoveryNotice (batch_recovery.sql:28-37, WHERE
	//     announced=1 AND notice_selected=0 — the seeded default):
	//     Notices == 1. No pending batch_control_requests exist, so
	//     Controls == 0.
	stopStore := session.NewBatchStore(app.DB())
	target := session.BatchControlTarget{
		Owner: "r12f-parent", Host: hostID, Root: rootID, Node: rootID,
	}
	// Fixture for the second (announced) root through the raw-schema helper
	// insertBatchNode (app_batch_cutover_test.go:47): owner is a fresh
	// session, host is the crashed helper host, announced=1 with a nonempty
	// announce_message_id (the batch_projection migration's CHECK requires it
	// for announced=1), state 'running' so the interrupt arm sees live nodes.
	const announcedRootID = "root-r12f-announced"
	_, aerr := db.New(app.DB()).CreateSession(ctx, db.CreateSessionParams{
		ID: "r12f-announced", Title: "r12f-announced",
	})
	require.NoError(t, aerr)
	announcedFixture := testBatchRoot("r12f-announced", announcedRootID,
		"call-r12f-announced", "running", 1)
	announcedFixture.host = hostID
	insertBatchNode(t, app, announcedFixture)
	out, recErr := stopStore.BatchRecoverDeadHost(ctx, hostID, session.BatchSharedHostProbe(dataDir))
	require.NoError(t, recErr)
	require.Equal(t, int64(1), out.DeletedRoots, "the unannounced root's subtree is deleted")
	require.Equal(t, int64(1), out.Interrupted, "the announced root is terminal-interrupted")
	require.Equal(t, int64(1), out.Notices, "the announced root's notice is selected")
	require.Zero(t, out.Controls, "no pending control requests existed")
	var nodes int
	require.NoError(t, app.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM batch_nodes WHERE root_id = ?`, rootID).Scan(&nodes))
	require.Zero(t, nodes, "the settled unannounced root leaves no rows (deleted, not 'interrupted')")
	var annState, annOutcome string
	require.NoError(t, app.DB().QueryRowContext(context.Background(),
		`SELECT state, outcome_state FROM batch_nodes WHERE root_id = ? AND node_id = root_id`,
		announcedRootID).Scan(&annState, &annOutcome))
	require.Equal(t, "interrupted", annState, "the announced root is persisted interrupted")
	require.Equal(t, "interrupted", annOutcome, "its outcome_state is latched interrupted")

	// The dead-host stop dispatch wires to recovery and returns BEFORE any
	// request creation (batch_control.go:62-66 returns the empty request
	// immediately): the unannounced root is gone, so batchControlTarget
	// (batch_control.go:23-27) finds no rows and reports
	// session.ErrBatchControlStale; the returned db.BatchControlRequest is
	// the zero value (RequestID == "").
	stopped, stopErr := stopStore.BatchRequestStop(ctx, target, session.BatchSharedHostProbe(dataDir))
	require.ErrorIs(t, stopErr, session.ErrBatchControlStale,
		"the documented dead-host result")
	require.Empty(t, stopped.RequestID, "the dead branch never creates a control request")

	// No replay: the second BatchRecoverDeadHost lists neither root anymore
	// (the unannounced one is deleted, the announced one is terminal and its
	// notice is already selected), so all four counters are zero. Both
	// persisted states are re-read FRESH (no stale local variables).
	out2, recErr2 := stopStore.BatchRecoverDeadHost(ctx, hostID, session.BatchSharedHostProbe(dataDir))
	require.NoError(t, recErr2)
	require.Zero(t, out2.DeletedRoots)
	require.Zero(t, out2.Interrupted)
	require.Zero(t, out2.Notices)
	require.Zero(t, out2.Controls)
	require.NoError(t, app.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM batch_nodes WHERE root_id = ?`, rootID).Scan(&nodes))
	require.Zero(t, nodes, "the unannounced root stays gone (nothing re-created)")
	require.NoError(t, app.DB().QueryRowContext(context.Background(),
		`SELECT state FROM batch_nodes WHERE root_id = ? AND node_id = root_id`,
		announcedRootID).Scan(&annState))
	require.Equal(t, "interrupted", annState, "the announced root stays interrupted (no replay)")

	res := app.ShutdownWithResult()
	require.Empty(t, res.CleanupErrors)
}

// TestLiveSessionDrivers_FlagOnLockProbe: with the flag ON the driver
// reader decides liveness through the shared lock probe, not the
// AsyncJobStore — same verdict table as the flag-off reader (own and sibling
// drivers kept, a provably dead host's marker dropped).
//
// Revert-check: pointing the flag-ON liveness back at
// app.asyncJobStore.HostLiveness fails on an App whose AsyncJobStore is nil
// (the store==nil guard) — asserted here by keeping the store non-nil but
// the probe reversion also flips the dead-host verdict only if the crash
// model differs; the nil-store reversion fails immediately.
func TestLiveSessionDrivers_FlagOnLockProbe(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	sess, err := h.app.Sessions.Create(ctx, "flagon")
	require.NoError(t, err)
	// Fixture: markers are written through the store's own claim path (the
	// session_drivers table), then the reader must never consult the store
	// again.
	require.NoError(t, h.app.asyncJobStore.ClaimSessionDriver(ctx, sess.ID))
	// The fixture drives the app's flag-OFF AsyncJobStore directly, which
	// lazily registers that store's OWN hosts/<id>.lock (ensureHost). A
	// flag-ON shutdown only releases the HostStore twin through the drain
	// gate (app_lifecycle.go's batchRuntime branch never touches
	// asyncJobStore), so this cleanup is the only release of that OS lock
	// handle; without it the open handle makes t.TempDir cleanup fail on
	// Windows (sharing violation on hosts/<id>.lock).
	t.Cleanup(func() { _ = h.app.asyncJobStore.Close(context.Background()) })
	siblingStore := session.NewAsyncJobStore(h.app.DB(), h.dataDir, 6161, "sibling")
	t.Cleanup(func() { _ = siblingStore.Close(context.Background()) })
	sibling, err := h.app.Sessions.Create(ctx, "flagon-sibling")
	require.NoError(t, err)
	require.NoError(t, siblingStore.ClaimSessionDriver(ctx, sibling.ID))
	deadStore := session.NewAsyncJobStore(h.app.DB(), h.dataDir, 4242, "dead")
	dead, err := h.app.Sessions.Create(ctx, "flagon-dead")
	require.NoError(t, err)
	require.NoError(t, deadStore.ClaimSessionDriver(ctx, dead.ID))
	require.NoError(t, deadStore.SimulateCrashForTest())

	got, err := h.app.LiveSessionDrivers(ctx)
	require.NoError(t, err)
	require.Contains(t, got, sess.ID, "own driver kept")
	require.Contains(t, got, sibling.ID, "live sibling driver kept")
	require.NotContains(t, got, dead.ID, "a provably dead host's marker keeps nothing open")
}

// TestLiveSessionDrivers_FlagOnWithoutAsyncJobStore: the flag-ON driver
// reader must produce a verdict for a host the flag-OFF liveness CANNOT —
// an App built with NO AsyncJobStore at all (the supported flag-ON shape,
// see TestBatchRuntimeFactoryFlagOnWithoutAsyncJobStore) has no
// HostLiveness to consult, and the store==nil closure's fail-open Unknown
// would RETAIN a provably dead host's driver marker.
//
// Divergence evidence (async_job_reader.go): the legacy HostLiveness never
// reads async_hosts heartbeats (they are display-only, sec.3.6) — for a
// non-nil store its verdicts agree with the shared lock probe, so no
// heartbeat divergence is constructible. The real divergence is the store
// itself: the shared probe answers HostStatusDead for the crashed foreign
// host, the legacy path on this App can only answer Unknown (alive).
//
// Revert-check: pointing the flag-ON liveness back at
// app.asyncJobStore.HostLiveness unconditionally panics on the nil store
// (HostLiveness derefs s.dataDir); guarding the reversion with the
// store==nil Unknown fallback keeps the dead host's marker live and fails
// the NotContains below.
func TestLiveSessionDrivers_FlagOnWithoutAsyncJobStore(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	// Foreign stores claim the markers; the App's own AsyncJobStore is then
	// removed — the flag-ON factory no longer requires it (R12-F).
	liveStore := session.NewAsyncJobStore(h.app.DB(), h.dataDir, 6161, "live-foreign")
	live, err := h.app.Sessions.Create(ctx, "flagon-nostore-live")
	require.NoError(t, err)
	require.NoError(t, liveStore.ClaimSessionDriver(ctx, live.ID))
	// The live foreign store keeps its OS lock for the whole test: only this
	// cleanup releases the handle (Windows sharing violation on
	// hosts/<id>.lock otherwise, same as the sibling cleanup above).
	t.Cleanup(func() { _ = liveStore.Close(context.Background()) })
	deadStore := session.NewAsyncJobStore(h.app.DB(), h.dataDir, 4242, "dead-foreign")
	dead, err := h.app.Sessions.Create(ctx, "flagon-nostore-dead")
	require.NoError(t, err)
	require.NoError(t, deadStore.ClaimSessionDriver(ctx, dead.ID))
	require.NoError(t, deadStore.SimulateCrashForTest())
	h.app.asyncJobStore = nil

	got, err := h.app.LiveSessionDrivers(ctx)
	require.NoError(t, err)
	require.Contains(t, got, live.ID, "the live foreign host's driver is kept by the shared probe")
	require.NotContains(t, got, dead.ID,
		"the probe's Dead verdict must drop the marker; the legacy store==nil Unknown fallback would keep it")
}

// TestDescribeOpenWorkFlagOn_BatchNodes: the wait heartbeat's flag-ON twin
// names live batch work from batch_nodes only — a live root row appears
// with its tool call id, and with no live rows the generic wording is used.
//
// Revert-check: reverting describeOpenWork's flag branch makes the heartbeat
// read ListRunningAsyncJobsForOwners again; under the dropped async_jobs
// table that errors into the generic wording, so the tool-call assertion
// below fails exactly when the async_jobs read is restored.
func TestDescribeOpenWorkFlagOn_BatchNodes(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	sess, err := h.app.Sessions.Create(ctx, "flagon-wait")
	require.NoError(t, err)
	// First real-schema assertion: the fixture INSERT fails on any violated
	// batch_nodes CHECK (root row: kind='batch', node_id=root_id,
	// tool_call_id/input_hash NOT NULL).
	insertBatchNode(t, h.app, testBatchRoot(sess.ID, "root-wait", "call-wait", "running", 0))

	l := &cliLoop{app: h.app, ctx: ctx, sessionID: sess.ID}
	got := l.describeOpenWork()
	require.Contains(t, got, "call-wait", "the heartbeat names the live batch work")

	// No live rows anywhere: generic wording.
	empty := &cliLoop{app: h.app, ctx: ctx, sessionID: "no-such-session"}
	require.Equal(t,
		"a running job/delegation, a host that is not provably dead, or an open wake schedule",
		empty.describeOpenWork())
}

// TestBatchNoticeStoreFlagOn_Wired: the flag-ON App notice store is built
// once, wired to the HostStore identity and a HostStore-backed liveness
// predicate (a dead host is "not alive", own host is alive).
//
// Revert-check: dropping the SetHostIDFunc/SetHostNotDeadFunc wiring fails
// the identity and verdict asserts (defaults are an empty id and
// equal-ids-only aliveness).
func TestBatchNoticeStoreFlagOn_Wired(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	a := batchFactoryApp(t)
	hostID, err := a.batchHostEnsure(context.Background())
	require.NoError(t, err)
	// The hand-assembled batchFactoryApp has no batch runtime, so Shutdown's
	// drain gate never runs; this cleanup is the release of the ensured
	// HostStore lock (the production branch added in releaseResources covers
	// the same shape on the real shutdown path). t.Cleanup is LIFO: this runs
	// AFTER the deadStore close below, but the two releases are independent.
	t.Cleanup(func() { _ = a.batchHostRelease(context.Background()) })
	ns := a.batchNoticeStore()
	require.NotNil(t, ns)
	require.Same(t, ns, a.BatchNoticeStore(), "memoized: one notice store per App")
	require.False(t, a.BatchNoticeStore() == nil)

	// A crashed foreign host is provably not alive for the notice keeper.
	deadStore := session.NewAsyncJobStore(a.DB(), a.dataDir, 4242, "dead")
	t.Cleanup(func() { _ = deadStore.Close(context.Background()) })
	deadHost, err := deadStore.EnsureHost(context.Background())
	require.NoError(t, err)
	require.NoError(t, deadStore.SimulateCrashForTest())
	// The verdicts are observable through the shared probe the wiring uses.
	st, err := session.ProbeHostShared(a.dataDir, deadHost)
	require.NoError(t, err)
	require.Equal(t, session.HostStatusDead, st)
	_, ownErr := session.ProbeHostShared(a.dataDir, hostID)
	require.ErrorIs(t, ownErr, session.ErrProbeOwnHost, "own host id is never probed")
}

// noticeWiredParams is the pull build callback: the same shape as the
// session package's sessionNoticeParams (notice_pull_test.go).
func noticeWiredParams(row session.SessionNoticeRow) message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: row.Text}},
		BackgroundJobNotice: true,
		NoticeKind:          row.Kind,
	}
}

// TestBatchNoticeStoreFlagOn_PullVerdictsThroughWiring: the RETURNED notice
// store runs its REAL pull surface (InsertSessionNotice ->
// PullSessionNotices -> sessionNoticeVoidCondition) through the App's
// HostStore wiring: identity (HostID) and liveness (SetHostNotDeadFunc
// probing foreign hosts; own id alive by equality).
//
// Trace per owner: batch_nodes root on the owner's host -> supervision
// notice -> PullSessionNotices: ListLiveBatchRootsForOwner -> wired
// hostNotDeadF per root -> keep (delivery 'done' + message) or void
// (delivery 'void', no message).
//
// Revert-check: dropping the SetHostIDFunc/SetHostNotDeadFunc wiring
// (app_batch_cutover_hosts.go) leaves the zero-value defaults — HostID()
// "" (first assert fails) and equal-ids-only aliveness with ownHostID="",
// which is false for EVERY host — so the live-foreign and own-host notices
// are voided and the Len/delivery asserts below fail exactly under the
// reversion (the dead-foreign case alone cannot catch it: both wiring and
// defaults void it).
func TestBatchNoticeStoreFlagOn_PullVerdictsThroughWiring(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	a := batchFactoryApp(t)
	ctx := context.Background()
	hostID, err := a.batchHostEnsure(ctx)
	require.NoError(t, err)
	// Same release pattern as TestBatchNoticeStoreFlagOn_Wired: the
	// hand-assembled app has no batch runtime drain gate.
	t.Cleanup(func() { _ = a.batchHostRelease(ctx) })
	ns := a.batchNoticeStore()
	require.NotNil(t, ns)
	require.Equal(t, hostID, ns.HostID(), "identity wiring: the store knows the ensured host")

	// A LIVE foreign host: holds its OS lock for the whole test (Close only
	// in cleanup, no SimulateCrashForTest).
	liveStore := session.NewAsyncJobStore(a.DB(), a.dataDir, 6161, "live-foreign")
	t.Cleanup(func() { _ = liveStore.Close(context.Background()) })
	liveHost, err := liveStore.EnsureHost(ctx)
	require.NoError(t, err)
	// A DEAD foreign host: crash-released lock, provably dead through the
	// probe the wiring uses.
	deadStore := session.NewAsyncJobStore(a.DB(), a.dataDir, 4242, "dead-foreign")
	t.Cleanup(func() { _ = deadStore.Close(context.Background()) })
	deadHost, err := deadStore.EnsureHost(ctx)
	require.NoError(t, err)
	require.NoError(t, deadStore.SimulateCrashForTest())
	st, err := session.ProbeHostShared(a.dataDir, deadHost)
	require.NoError(t, err)
	require.Equal(t, session.HostStatusDead, st, "fixture: the dead foreign host is provably dead")

	// One owner + one live batch root per host; the supervision void
	// condition is per-owner (any not-dead live root keeps the notice), so
	// the three verdicts need three owners.
	q := db.New(a.DB())
	seed := func(owner, id, host string) {
		_, err := q.CreateSession(ctx, db.CreateSessionParams{ID: owner, Title: owner})
		require.NoError(t, err)
		f := testBatchRoot(owner, id, "call-"+id, "running", 0)
		f.host = host
		insertBatchNode(t, a, f)
		require.NoError(t, ns.InsertSessionNotice(ctx, owner, session.NoticeKindSupervision, "still working on "+id, true, ""))
	}
	seed("wired-owner-live", "root-wired-live", liveHost)
	seed("wired-owner-dead", "root-wired-dead", deadHost)
	seed("wired-owner-own", "root-wired-own", hostID)

	pull := func(owner string) []session.PulledNotice {
		pulled, err := ns.PullSessionNotices(ctx, message.NewService(q), owner, noticeWiredParams)
		require.NoError(t, err)
		return pulled
	}
	delivery := func(owner string) string {
		rows, err := q.ListSessionNoticesForOwner(ctx, owner)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		return rows[0].Delivery
	}

	require.Len(t, pull("wired-owner-live"), 1,
		"a live root on a live foreign host keeps the notice (wired probe: not dead)")
	require.Equal(t, "done", delivery("wired-owner-live"))
	require.Empty(t, pull("wired-owner-dead"),
		"a live root on a provably dead foreign host voids the notice")
	require.Equal(t, "void", delivery("wired-owner-dead"),
		"the void rule must have fired: a swallowed error would leave the row pending")
	require.Len(t, pull("wired-owner-own"), 1,
		"the own host is alive by identity, never probed")
	require.Equal(t, "done", delivery("wired-owner-own"))
}

// TestBatchRuntimeFactoryFlagOnWithoutAsyncJobStore: with the flag ON the
// factory no longer requires the AsyncJobStore — a hand-assembled App with
// only the HostStore resources builds, and the identity falls back to the
// HostStore's. Flag OFF keeps the original fail-closed rule.
//
// Revert-check: restoring `if a.asyncJobStore == nil` unconditionally makes
// the flag-ON half error; dropping the flag guard on the fallback makes the
// flag-OFF half panic on the nil store.
func TestBatchRuntimeFactoryFlagOnWithoutAsyncJobStore(t *testing.T) {
	t.Run("flag on: HostStore identity suffices", func(t *testing.T) {
		t.Setenv("RUSH_BATCH_RUNTIME", "true")
		a := batchFactoryApp(t)
		a.asyncJobStore = nil
		_, err := a.batchHostEnsure(context.Background())
		require.NoError(t, err)
		// Same shape as TestBatchNoticeStoreFlagOn_Wired: no batch runtime, so
		// the drain gate never runs — release the ensured HostStore lock here.
		t.Cleanup(func() { _ = a.batchHostRelease(context.Background()) })
		factory, err := newBatchRuntimeFactory(a, batchFactoryDeps())
		require.NoError(t, err)
		require.Equal(t, "host", factory.deps.Host, "explicit identity still wins")
		factory2, err := newBatchRuntimeFactory(a, agent.BatchInstallDeps{RunnerID: "r", Owner: "o"})
		require.NoError(t, err)
		require.Equal(t, a.BatchHostStore().HostID(), factory2.deps.Host,
			"identity falls back to the HostStore's")
	})
	t.Run("flag off: still fails closed without the store", func(t *testing.T) {
		a := batchFactoryApp(t)
		a.asyncJobStore = nil
		_, err := newBatchRuntimeFactory(a, batchFactoryDeps())
		require.Error(t, err, "flag off: no agent store stays an error")
	})
}
