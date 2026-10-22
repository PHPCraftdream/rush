// R12-F chunk C tests: the host identity and
// teardown are independent of the async_jobs table (session.HostStore), the
// shutdown drain completes, a REAL cross-process locked-host fault recovers,
// and the readers (drivers, wait heartbeat) use batch_nodes plus the
// shared lock probe.

package app

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// TestBatchHostStore_TeardownWithoutAsyncJobsTable (R12-F acceptance
// 4 of the decomposition): a REAL app built through New
// against a real SQLite data dir, then DROP TABLE IF EXISTS async_jobs (the
// table is gone in the final world; the drop proves the teardown never
// queries it), then the real teardown through ShutdownWithResult must end
// with no cleanup errors: the Close path releases the host identity through
// the HostStore's DeleteAsyncHostIfNoNodes.
//
// Revert-check: pointing the install/telemetry back at the
// AsyncJobStore makes EnsureHost fail at New (install) or the teardown's
// DeleteAsyncHostIfNoJobs fail after the drop — either surfaces as a
// non-empty CleanupErrors or a New error.
func TestBatchHostStore_TeardownWithoutAsyncJobsTable(t *testing.T) {
	app := newFullNewApp(t)
	require.NotNil(t, app.batchRuntime, "runtime installed")
	hostID := app.BatchHostStore().HostID()
	require.NotEmpty(t, hostID)
	// First real-schema assertion: the HostStore wrote the async_hosts row
	// (the identity's durable bookkeeping).
	var hosts int
	require.NoError(t, app.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM async_hosts WHERE id = ?`, hostID).Scan(&hosts))
	require.Equal(t, 1, hosts, "the HostStore registered async_hosts row")

	// The cutover end-state: the table is GONE; only the batch authority can
	// survive it.
	_, err := app.DB().ExecContext(context.Background(), `DROP TABLE IF EXISTS async_jobs`)
	require.NoError(t, err)

	res := app.ShutdownWithResult()
	require.False(t, res.Forced)
	require.Empty(t, res.CleanupErrors,
		"the teardown never queries async_jobs")

	// All-OS leak oracle: after the real shutdown, EVERY lock file
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

// TestBatchHostStore_ShutdownDrainCompletes (R12-F acceptance: the
// shutdown drain completes): the real gate (DrainAndRelease) runs
// through ShutdownWithResult and the HostStore release finishes gracefully —
// no forced verdict, no cleanup errors.
//
// Revert-check: wiring RetainHostIdentity/ReleaseHostIdentity back to a
// store that keeps async_jobs alive keeps this green only while async_jobs
// exists; combined with the table-drop test above it pins the HostStore
// wiring, and reverting the gate itself (nil release seams) fails the
// Forced/CleanupErrors asserts.
func TestBatchHostStore_ShutdownDrainCompletes(t *testing.T) {
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

// DELETED with H4: TestBatchHostShutdownReleasesEveryHostFlagOn -- the
// legacy AsyncJobStore host identity (and releaseResources' legacy-release
// block) is gone; the "release BOTH hosts" scenario is obsolete. The
// HostStore-only release contract is pinned by
// TestBatchHostStore_ShutdownDrainCompletes and
// TestBatchHostStore_TeardownWithoutAsyncJobsTable.
// TestBatchHostLockSubprocessHelper is the guarded helper process: it
// registers a REAL host (OS lock on <dataDir>/hosts/<id>.lock plus the
// async_hosts row) against the data dir named by the environment and blocks
// forever holding the lock. After registering it writes a structured record
// to the id file (bare host id on the first line, then pid/path/identity/
// registered_at key=value lines) so the parent's diagnostics can compare
// the helper-reported lock path identity with what the parent observes. It
// only ever runs when the test binary is re-invoked with
// R12F_HOST_LOCK_HELPER=1 (see the parent below).
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
	// Mirror production's lock-path derivation (HostIdentity exposes only ID
	// and DataDir; the lock handle is unexported, so derive it from the
	// data dir) and record the file's identity AFTER registration.
	lockPath := session.HostLockPath(dataDir, h.ID)
	record := h.ID + "\n" +
		"pid=" + strconv.Itoa(os.Getpid()) + "\n" +
		"path=" + lockPath + "\n" +
		"identity=" + hostFileIdentity(lockPath) + "\n" +
		"registered_at=" + time.Now().UTC().Format(time.RFC3339Nano) + "\n"
	if err := os.WriteFile(idFile, []byte(record), 0o644); err != nil {
		_ = db.Release(dataDir)
		os.Exit(5)
	}
	// Hold the OS host lock until the parent kills us. NOT `select{}`: the
	// helper binary runs ONLY this test, so once this test function blocks in
	// select{} every goroutine in the process is blocked forever and Go's
	// deadlock detector throws "all goroutines are asleep - deadlock!",
	// terminating the helper — and silently DROPPING its OS lock — before the
	// parent ever probes (the observed macOS failure: expected Alive, got
	// Dead). A sleeping timer keeps a pending timer, so the detector never
	// fires and the lock is released only by the parent's kill.
	for {
		time.Sleep(time.Hour)
	}
}

// insertBatchNodeOnConn mirrors insertBatchNode (app_batch_cutover_test.go:47)
// exactly — same columns, same params, delivery defaulting to 'none' — but
// takes a raw *sql.DB handle: at seed time (before the crash gate) no App
// exists yet over the shared data dir, only the throwaway seedConn.
func insertBatchNodeOnConn(t *testing.T, conn *sql.DB, f batchNodeFixture) {
	t.Helper()
	// delivery defaults to the schema default 'none': every unselected fixture
	// row must carry a CHECK-legal value (the delivery CHECK rejects '').
	if f.delivery == "" {
		f.delivery = "none"
	}
	_, err := conn.ExecContext(context.Background(), `
INSERT INTO batch_nodes (
	node_id, root_id, owner_session_id, host_id, parent_id, ordinal, kind,
	tool_call_id, input_hash, announced, announce_message_id, mode, task_kind,
	child_session_id, state, notice_selected, notice_kind, notice_state,
	notice_cause, notice_sequence, notice_ordinal, delivery, wake, reacted,
	outcome_summary, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, strftime('%s','now'), strftime('%s','now'))`,
		f.nodeID, f.rootID, f.owner, f.host, f.parent, f.ordinal, f.kind,
		f.toolCallID, f.inputHash, f.announced, f.announceMessageID, f.mode, f.taskKind,
		f.childSessionID, f.state, f.noticeSelected, f.noticeKind, f.noticeState,
		f.noticeCause, f.noticeSequence, f.noticeOrd, f.delivery, f.wake, f.reacted,
		f.outcomeSummary)
	require.NoError(t, err)
}

// helperOutputBuffer is a goroutine-safe sink for exec.Cmd output: the
// command's copy goroutines write while the failure diagnostics read, and
// the test reaps the helper through a single cmd.Wait run by the
// helperWaitResult goroutine (which does not join the copy goroutines).
type helperOutputBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *helperOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *helperOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// helperWaitResult owns the helper's SINGLE cmd.Wait: run is started once in
// a goroutine right after cmd.Start, and both the cleanup and the crash
// section drain the same result through doneCh, so Wait is never called
// twice on the process.
type helperWaitResult struct {
	mu       sync.Mutex
	done     bool
	err      error
	finished time.Time
	doneCh   chan struct{}
}

func newHelperWaitResult() *helperWaitResult {
	return &helperWaitResult{doneCh: make(chan struct{})}
}

func (w *helperWaitResult) run(cmd *exec.Cmd) {
	err := cmd.Wait()
	w.mu.Lock()
	w.done = true
	w.err = err
	w.finished = time.Now()
	w.mu.Unlock()
	close(w.doneCh)
}

// helperWaitState is a consistent view of the helper's wait result.
type helperWaitState struct {
	done     bool
	err      error
	finished time.Time
}

// snapshot reports the current wait state without racing the run goroutine.
func (w *helperWaitResult) snapshot() helperWaitState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return helperWaitState{done: w.done, err: w.err, finished: w.finished}
}

// hostProbeTimeline records when each phase of the parent's test ran.
type hostProbeTimeline struct {
	start, idFile, dbOpened, seeded, firstProbe time.Time
}

// helperRecord is the structured tail of the id file: key=value lines parsed
// by parseHelperRecord (all fields may stay empty for an id-only file).
type helperRecord struct {
	Pid, Path, Identity, RegisteredAt string
}

// parseHelperRecord splits the helper's id file into the bare host id (the
// trimmed first line) and the optional key=value record lines; a missing or
// blank tail yields a zero helperRecord so the parent stays compatible with
// an id-only file.
func parseHelperRecord(data []byte) (string, helperRecord) {
	lines := strings.Split(string(data), "\n")
	var rec helperRecord
	if len(lines) == 0 {
		return "", rec
	}
	hostID := strings.TrimSpace(lines[0])
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "pid":
			rec.Pid = v
		case "path":
			rec.Path = v
		case "identity":
			rec.Identity = v
		case "registered_at":
			rec.RegisteredAt = v
		}
	}
	return hostID, rec
}

// sameHostIdentity compares the helper-registered file identity with the
// parent's current one for the failure report.
func sameHostIdentity(registered, current string) string {
	if registered == "" {
		return "unknown (helper reported no identity)"
	}
	return strconv.FormatBool(registered == current)
}

// lockedHostFaultDiagnostics builds the failure message for a lost helper
// lock: the helper's captured output and structured record, its wait state
// (through the single cmd.Wait owned by hw) plus a Signal(0) poll on
// platforms that support it, the recorded timeline, the helper-reported lock
// path and identity against the parent's own os.Stat (immediately before AND
// after the probe), the canonical vs literal path spelling, and one
// EXCLUSIVE ProbeHostLock whose won lock is Released immediately so the test
// state is not corrupted.
func lockedHostFaultDiagnostics(ctx context.Context, cmd *exec.Cmd, hw *helperWaitResult, rec helperRecord, tl hostProbeTimeline, helperStdout, helperStderr *helperOutputBuffer, dataDir, hostID string) string {
	lockPath := session.HostLockPath(dataDir, hostID)
	if rec.Path != "" {
		lockPath = rec.Path
	}
	report := fmt.Sprintf("helper stdout: %q\nhelper stderr: %q\n", helperStdout.String(), helperStderr.String())
	if st := hw.snapshot(); st.done {
		report += fmt.Sprintf("helper exited at %v (elapsed since start %v): %v\n", st.finished, st.finished.Sub(tl.start), st.err)
	} else {
		report += "helper has not exited (cmd.Wait still pending)\n"
	}
	if runtime.GOOS != "windows" {
		// After Wait has run the signal typically fails with "process
		// already finished", which is itself informative here.
		if serr := cmd.Process.Signal(syscall.Signal(0)); serr != nil {
			report += "helper Signal(0): " + serr.Error() + " (typical after Wait)\n"
		} else {
			report += "helper Signal(0): ok (process still exists)\n"
		}
	}
	since := func(ts time.Time) time.Duration { return ts.Sub(tl.start) }
	report += fmt.Sprintf("timeline since start: idfile +%v, dbopened +%v, seeded +%v, firstprobe +%v; ctx err: %v\n",
		since(tl.idFile), since(tl.dbOpened), since(tl.seeded), since(tl.firstProbe), ctx.Err())
	report += fmt.Sprintf("helper-reported path: %q\nhelper-reported identity: %q (pid %q, registered_at %q)\n", rec.Path, rec.Identity, rec.Pid, rec.RegisteredAt)
	describe := func(label string) string {
		info, serr := os.Stat(lockPath)
		switch {
		case serr == nil:
			id := hostFileIdentity(lockPath)
			return fmt.Sprintf("%s: exists, size %d bytes, identity %q, same as registered: %s\n", label, info.Size(), id, sameHostIdentity(rec.Identity, id))
		case os.IsNotExist(serr):
			return label + ": ENOENT (missing)\n"
		default:
			return label + ": " + serr.Error() + "\n"
		}
	}
	report += describe("parent stat before probe")
	canonical, cerr := filepath.EvalSymlinks(lockPath)
	report += fmt.Sprintf("path spellings: literal %q, canonical %q (eval err %v), differ: %v\n",
		lockPath, canonical, cerr, cerr == nil && canonical != lockPath)
	stX, lockX, errX := session.ProbeHostLock(lockPath)
	report += fmt.Sprintf("exclusive ProbeHostLock %v after start: verdict %v, lock won %v, err %v; helper alive=%v\n",
		time.Since(tl.start), stX, lockX != nil, errX, !hw.snapshot().done)
	if lockX != nil {
		_ = lockX.Release()
	}
	report += describe("parent stat after probe")
	return report
}

// TestBatchHostStore_LockedHostFaultThenRecovery (R12-F acceptance:
// "real locked-host fault subprocess plus subsequent recovery"): a real
// subprocess holds a real host lock in a shared data dir; the parent probes
// it ALIVE through the shared lock probe (a REAL lock the parent cannot
// take), kills the holder (the crash model — the OS lock dies with the
// process), probes it DEAD, and then a REAL app built over the same
// data dir drives the production dead-host recovery through the PUBLIC stop
// dispatch before shutting down cleanly.
//
// Coverage: TWO batch roots are seeded through real persisted rows BEFORE
// the crash gate (while the helper host is still ALIVE): the unannounced
// root via the real BatchStore.Reserve path and an ANNOUNCED root via the
// raw-INSERT helper insertBatchNodeOnConn (mirrors insertBatchNode exactly;
// announced=1 carries a nonempty announce_message_id for the schema CHECK).
// The recovery is then driven by BatchRequestStop (internal/session/
// batch_control.go): for a provably dead host the switch at :63-65
// dispatches to BatchRecoverDeadHost and returns ErrBatchControlStale
// BEFORE creating any control request. Removing that dead-host dispatch
// leaves BOTH roots 'running' and fails the persisted-state asserts below.
// The two persisted arms are distinguished from each other: the unannounced
// root's whole subtree is DELETED (zero batch_nodes rows for its root_id)
// while the announced root takes the terminal-INTERRUPTION arm
// (state='interrupted', outcome_state='interrupted') and still exists — that
// pair separates the delete arm from the interrupt arm. A follow-up
// BatchRequestStop on the ANNOUNCED (now terminal 'interrupted') target
// returns session.ErrBatchControlStale via the !batchStateLive(root.State)
// arm (batch_control.go:38-39; batchStateLive accepts only
// pending/starting/running/cancelling, batch_store.go:265-267) — pinning
// stale-after-terminal. The outcome counters are NOT asserted for the
// dispatched recovery itself (BatchRequestStop does not return them); the
// persisted-row asserts above are the stronger oracle. A second direct
// BatchRecoverDeadHost over the already-recovered host proves NO REPLAY:
// all four counters of BatchRecoveryOutcome (batch_recovery.go:21) are
// zero and both persisted states are re-read fresh (the unannounced root
// stays gone, the announced stays 'interrupted').
//
// Revert-check: removing the HostStatusDead -> BatchRecoverDeadHost branch
// at internal/session/batch_control.go leaves BOTH roots 'running' and
// fails the persisted-state asserts; removing batchRecoverRoot's
// DeleteBatchSubtree arm leaves the unannounced root present (fails its
// zero-row assert); removing the interrupt arm leaves the announced root
// 'running' (fails its 'interrupted' assert). Reverting the HostStore probe
// wiring (e.g. liveness through a store that never sees the foreign lock)
// fails the Alive/Dead asserts.

func TestBatchHostStore_LockedHostFaultThenRecovery(t *testing.T) {
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
	// Capture the helper's output so a lock-loss failure can show why the
	// helper ended (a deadlock-detector throw lands on stderr).
	var helperStdout, helperStderr helperOutputBuffer
	cmd.Stdout = &helperStdout
	cmd.Stderr = &helperStderr
	require.NoError(t, cmd.Start())
	tl := hostProbeTimeline{start: time.Now()}
	hw := newHelperWaitResult()
	go hw.run(cmd)
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-hw.doneCh })

	// Wait for the helper's REAL registration (it writes the id only after
	// RegisterHost took the OS lock).
	var hostID string
	var rec helperRecord
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, rerr := os.ReadFile(idFile)
		if rerr == nil && len(b) > 0 {
			hostID, rec = parseHelperRecord(b)
			if hostID != "" {
				tl.idFile = time.Now()
				break
			}
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
	tl.dbOpened = time.Now()
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

	// Second (ANNOUNCED) root, seeded in the SAME pre-crash block while the
	// helper host is still ALIVE: owner is a fresh session, host is the
	// crashed helper host, announced=1 with a nonempty announce_message_id
	// (the batch_projection migration's CHECK requires it for announced=1),
	// state 'running' so the interrupt arm sees live nodes. No App exists
	// yet, so the raw-INSERT helper insertBatchNodeOnConn (declared above,
	// mirroring insertBatchNode) writes it through seedConn.
	const announcedRootID = "root-r12f-announced"
	_, aerr := seedQueries.CreateSession(ctx, db.CreateSessionParams{
		ID: "r12f-announced", Title: "r12f-announced",
	})
	require.NoError(t, aerr)
	announcedFixture := testBatchRoot("r12f-announced", announcedRootID,
		"call-r12f-announced", "running", 1)
	announcedFixture.host = hostID
	insertBatchNodeOnConn(t, seedConn, announcedFixture)
	tl.seeded = time.Now()

	// The locked-host fault, probed for real: the parent cannot win the
	// lock while the helper lives.
	tl.firstProbe = time.Now()
	st, err := session.ProbeHostShared(dataDir, hostID)
	require.NoError(t, err)
	if st != session.HostStatusAlive {
		t.Fatalf("helper does not hold the real host lock: shared probe verdict %v\n%s",
			st, lockedHostFaultDiagnostics(ctx, cmd, hw, rec, tl, &helperStdout, &helperStderr, dataDir, hostID))
	}

	// Crash: kill -9 equivalent. The OS lock dies with the process.
	require.NoError(t, cmd.Process.Kill())
	<-hw.doneCh
	st, err = session.ProbeHostShared(dataDir, hostID)
	require.NoError(t, err)
	require.Equal(t, session.HostStatusDead, st, "crashed host is provably dead")

	// Recovery: a real app over the same data dir installs
	// Recovery: a real app over the same data dir installs
	// and irrelevant) and tears down cleanly through the real gate.
	app := newFullNewAppAt(t, dataDir)
	got := app.BatchHostStore().HostID()
	require.NotEmpty(t, got)
	require.NotEqual(t, hostID, got, "recovered app runs on its own host identity")
	var hosts int
	require.NoError(t, app.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM async_hosts WHERE id = ?`, got).Scan(&hosts))
	require.Equal(t, 1, hosts, "first real-schema assertion: recovered HostStore row")

	// The recovery is driven through the PUBLIC stop dispatch, not the
	// direct BatchRecoverDeadHost entry: BatchRequestStop (internal/session/
	// batch_control.go:48) probes the crashed host DEAD and the switch at
	// :63-65 calls BatchRecoverDeadHost for BOTH seeded roots, then returns
	// the zero request joined with session.ErrBatchControlStale — BEFORE any
	// control request is created and BEFORE the caller sees the outcome
	// counters. Both roots were seeded 'running' BEFORE the crash gate, so
	// BatchRecoveryRoots (batch_recovery.sql:3-9) lists both: the unannounced
	// root takes batchRecoverRoot's DeleteBatchSubtree arm
	// (batch_recovery.go:74-75) and the announced root takes the
	// terminal-interruption arm (batch_recovery.go:77+ via
	// BatchRecoveryInterrupt, batch_recovery.sql:18-26).
	stopStore := session.NewBatchStore(app.DB())
	targetUnannounced := session.BatchControlTarget{
		Owner: "r12f-parent", Host: hostID, Root: rootID, Node: rootID,
	}
	stopped, stopErr := stopStore.BatchRequestStop(ctx, targetUnannounced, session.BatchSharedHostProbe(dataDir))
	require.ErrorIs(t, stopErr, session.ErrBatchControlStale,
		"the dead-host dispatch result: recovery ran, no request created")
	require.Empty(t, stopped.RequestID, "the dead branch never creates a control request")

	// The dispatch mutation is caught from PERSISTED ROWS: the unannounced
	// root's subtree is DELETED (zero rows) while the announced root still
	// EXISTS terminal-'interrupted' — that pair distinguishes the delete arm
	// from the interrupt arm (any-recovery alone cannot).
	var nodes int
	require.NoError(t, app.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM batch_nodes WHERE root_id = ?`, rootID).Scan(&nodes))
	require.Zero(t, nodes, "the dispatched recovery deleted the unannounced root's subtree")
	var annState, annOutcome string
	require.NoError(t, app.DB().QueryRowContext(context.Background(),
		`SELECT state, outcome_state FROM batch_nodes WHERE root_id = ? AND node_id = root_id`,
		announcedRootID).Scan(&annState, &annOutcome))
	require.Equal(t, "interrupted", annState, "the announced root is persisted interrupted")
	require.Equal(t, "interrupted", annOutcome, "its outcome_state is latched interrupted")

	// Follow-up stop on the ANNOUNCED target (owner r12f-announced): the
	// root is now terminal ('interrupted'), so batchControlTarget returns
	// session.ErrBatchControlStale via the !batchStateLive(root.State) arm
	// (batch_control.go:38-39; batchStateLive accepts only
	// pending/starting/running/cancelling, batch_store.go:265-267) —
	// stale-after-terminal, without any further transition.
	targetAnnounced := session.BatchControlTarget{
		Owner: "r12f-announced", Host: hostID, Root: announcedRootID, Node: announcedRootID,
	}
	stopped2, stopErr2 := stopStore.BatchRequestStop(ctx, targetAnnounced, session.BatchSharedHostProbe(dataDir))
	require.ErrorIs(t, stopErr2, session.ErrBatchControlStale,
		"terminal roots are stale targets (batchStateLive excludes 'interrupted')")
	require.Empty(t, stopped2.RequestID, "no control request against a terminal root")

	// No replay: a second, direct BatchRecoverDeadHost over the
	// already-recovered host lists neither root anymore (the unannounced one
	// is deleted, the announced one is terminal and its notice is already
	// selected), so all four counters of BatchRecoveryOutcome
	// (batch_recovery.go:21) are zero — this is the ONLY place the counters
	// are asserted (the dispatched call returns none; the persisted-row
	// asserts above are the recovery oracle). Both persisted states are
	// re-read FRESH (no stale local variables).
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

// TestLiveSessionDrivers_LockProbe: the driver reader decides liveness
// through the shared lock probe (own and sibling
// drivers kept, a provably dead host's marker dropped).
//
// Revert-check: pointing the liveness back at a legacy store-backed
// HostLiveness (or a store==nil Unknown fallback) flips the dead-host
// verdict and fails the NotContains below.
func TestLiveSessionDrivers_LockProbe(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	sess, err := h.app.Sessions.Create(ctx, "flagon")
	require.NoError(t, err)
	// Fixture: markers are written through the host store's own claim path
	// (the session_drivers table), then the reader must never consult the
	// store again. The App's Shutdown cleanup (harness) releases the own
	// host identity through the drain gate.
	require.NoError(t, h.app.BatchHostStore().ClaimSessionDriver(ctx, sess.ID))
	siblingStore := session.NewHostStore(h.app.DB(), h.dataDir, 6161, "sibling")
	t.Cleanup(func() { _ = siblingStore.Close(context.Background()) })
	sibling, err := h.app.Sessions.Create(ctx, "flagon-sibling")
	require.NoError(t, err)
	require.NoError(t, siblingStore.ClaimSessionDriver(ctx, sibling.ID))
	deadStore := session.NewHostStore(h.app.DB(), h.dataDir, 4242, "dead")
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

// DELETED with H4: TestLiveSessionDrivers_FlagOnWithoutAsyncJobStore -- the
// premise (an App carrying a legacy AsyncJobStore whose nil fallback
// answers Unknown) no longer exists: the shared lock probe is the only
// liveness authority and the probe-only verdicts are pinned by
// TestLiveSessionDrivers_LockProbe.
//
// TestDescribeOpenWork_BatchNodes: the wait heartbeat names live batch work
// from batch_nodes only — a live root row appears with its tool call id, and
// with no live rows the generic wording is used.
//
// Revert-check: reverting describeOpenWork to a legacy
// ListRunningAsyncJobsForOwners read fails the tool-call assertion below.
func TestDescribeOpenWork_BatchNodes(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	sess, err := h.app.Sessions.Create(ctx, "flagon-wait")
	require.NoError(t, err)
	f6ReserveDelegation(t, h.app, sess.ID, "call-wait", "", "shell", "running")

	l := &cliLoop{app: h.app, ctx: ctx, sessionID: sess.ID}
	got := l.describeOpenWork()
	require.Contains(t, got, "call-wait", "the heartbeat names the live batch work")

	// No live rows anywhere: generic wording.
	empty := &cliLoop{app: h.app, ctx: ctx, sessionID: "no-such-session"}
	require.Equal(t,
		"a running job/delegation, a host that is not provably dead, or an open wake schedule",
		empty.describeOpenWork())
}

// TestBatchNoticeStore_Wired: the App notice store is built
// once, wired to the HostStore identity and a HostStore-backed liveness
// predicate (a dead host is "not alive", own host is alive).
//
// Revert-check: dropping the SetHostIDFunc/SetHostNotDeadFunc wiring fails
// the identity and verdict asserts (defaults are an empty id and
// equal-ids-only aliveness).
func TestBatchNoticeStore_Wired(t *testing.T) {
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
	deadStore := session.NewHostStore(a.DB(), a.dataDir, 4242, "dead")
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

// TestBatchNoticeStore_PullVerdictsThroughWiring: the RETURNED notice
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
func TestBatchNoticeStore_PullVerdictsThroughWiring(t *testing.T) {
	a := batchFactoryApp(t)
	ctx := context.Background()
	hostID, err := a.batchHostEnsure(ctx)
	require.NoError(t, err)
	// Same release pattern as TestBatchNoticeStore_Wired: the
	// hand-assembled app has no batch runtime drain gate.
	t.Cleanup(func() { _ = a.batchHostRelease(ctx) })
	ns := a.batchNoticeStore()
	require.NotNil(t, ns)
	require.Equal(t, hostID, ns.HostID(), "identity wiring: the store knows the ensured host")

	// A LIVE foreign host: holds its OS lock for the whole test (Close only
	// in cleanup, no SimulateCrashForTest).
	liveStore := session.NewHostStore(a.DB(), a.dataDir, 6161, "live-foreign")
	t.Cleanup(func() { _ = liveStore.Close(context.Background()) })
	liveHost, err := liveStore.EnsureHost(ctx)
	require.NoError(t, err)
	// A DEAD foreign host: crash-released lock, provably dead through the
	// probe the wiring uses.
	deadStore := session.NewHostStore(a.DB(), a.dataDir, 4242, "dead-foreign")
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

// TestBatchRuntimeFactoryHostStoreIdentitySuffices: the factory requires
// only the HostStore resources — a hand-assembled App with
// no other batch state builds, and the identity falls back to the
// HostStore's. Without a host store it fails closed (pinned by
// TestBatchRuntimeFactoryFailsClosedWithoutResources).
func TestBatchRuntimeFactoryHostStoreIdentitySuffices(t *testing.T) {
	a := batchFactoryApp(t)
	_, err := a.batchHostEnsure(context.Background())
	require.NoError(t, err)
	// Same shape as TestBatchNoticeStore_Wired: no batch runtime, so
	// the drain gate never runs — release the ensured HostStore lock here.
	t.Cleanup(func() { _ = a.batchHostRelease(context.Background()) })
	factory, err := newBatchRuntimeFactory(a, batchFactoryDeps())
	require.NoError(t, err)
	require.Equal(t, "host", factory.deps.Host, "explicit identity still wins")
	factory2, err := newBatchRuntimeFactory(a, agent.BatchInstallDeps{RunnerID: "r", Owner: "o"})
	require.NoError(t, err)
	require.Equal(t, a.BatchHostStore().HostID(), factory2.deps.Host,
		"identity falls back to the HostStore's")
}
