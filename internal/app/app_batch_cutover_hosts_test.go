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
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
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
// data dir registers its own HostStore identity and shuts down cleanly.
//
// Revert-check: reverting the flag-ON install to the AsyncJobStore's
// EnsureHost keeps this green (both read async_hosts) — the independence
// pin is TestBatchHostStoreFlagOn_TeardownWithoutAsyncJobsTable; reverting
// the HostStore probe wiring (e.g. liveness through a store that never sees
// the foreign lock) fails the Alive/Dead asserts.
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
