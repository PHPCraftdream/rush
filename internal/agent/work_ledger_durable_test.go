// Phase-4 step 2 coverage (docs/plans/2026-09-28-async-phase4-durable-core.md
// sec.5 step 2, sec.6): transition is the single writer of a non-sync async
// job's terminal state, backed by a real temp-SQLite AsyncJobStore. Covers
// DUR-1a (busy-DB retry), the in-flight latch, DUR-1c (each cause records
// its own state/notice_kind/wake), DUR-1d (the shutdown latch), and DUR-8
// (fail-closed Start). DUR-1b ("the loser adopts the state") is covered at
// the store layer (internal/session/async_job_store_test.go,
// TestAsyncJobStore_TransitionWonThenLostThenGone) -- the CAS race it
// describes is a cross-host/cross-process race; within one workLedger the
// in-memory "transitioning" latch already makes two DIFFERENT causes for
// the SAME job mutually exclusive, so this file's latch test is the
// same-process manifestation of that guarantee (exactly one cause is ever
// written; the other is skipped, not raced).
package agent

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// lowBusyTimeoutStore builds an AsyncJobStore over a SEPARATE raw connection
// to the SAME on-disk file as base, opened with a near-zero busy_timeout so
// SQLITE_BUSY surfaces immediately as an error instead of blocking inside
// the driver -- the only way to actually exercise workLedger.transition's
// OWN growing-backoff retry loop (doc sec.3.1) rather than merely relying on
// SQLite's own busy_timeout pragma to block-and-succeed on the first call.
func lowBusyTimeoutStore(t *testing.T, dataDir string) *session.AsyncJobStore {
	t.Helper()
	path := filepath.Join(dataDir, "rush.db")
	conn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(20)")
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	_, err = conn.ExecContext(context.Background(), `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	store := session.NewAsyncJobStore(conn, dataDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// holdWriteLock opens its own raw connection to path and holds an open
// write transaction against an unrelated table for hold, releasing it (and
// closing the connection) when it returns. Runs synchronously -- callers
// that need it concurrent with other work launch it in a goroutine.
func holdWriteLock(t *testing.T, dataDir string, hold time.Duration) {
	t.Helper()
	path := filepath.Join(dataDir, "rush.db")
	conn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	require.NoError(t, err)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO async_hosts (id, pid, label, started_at) VALUES (?, 1, '', 0)`, uniqueHostRowID())
	require.NoError(t, err)
	time.Sleep(hold)
	require.NoError(t, tx.Commit())
}

// uniqueHostRowID returns a fresh id for holdWriteLock's throwaway
// async_hosts row (its PRIMARY KEY must not collide across calls). Each
// caller uses its own on-disk file (t.TempDir()), so this only needs to be
// unique WITHIN one such file/call, which UnixNano already guarantees
// without any shared mutable state (avoiding a data race under t.Parallel).
func uniqueHostRowID() string {
	return fmt.Sprintf("busy-holder-%d", time.Now().UnixNano())
}

// TestWorkLedger_TransitionRetriesUnderBusyDBThenCommits is DUR-1a: a
// transition against a busy DB (a second connection holding a write
// transaction for a few hundred ms) retries with growing backoff and
// eventually commits, rather than giving up or blocking inside a single
// driver call.
//
// Revert check performed: temporarily replaced the retry loop's
// `time.Sleep(asyncStoreRetryBackoff(attempt))` with `return` (abandoning
// on the first busy error) -- this test FAILED (finish's own DB write never
// committed, no completion was ever delivered, the test's require.Eventually-
// free direct wait timed out). Restored the loop; re-ran, passed. Diffed
// work_ledger_transition.go against git HEAD after restoring: no diff.
func TestWorkLedger_TransitionRetriesUnderBusyDBThenCommits(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	db.Release(dataDir) // release THIS handle; lowBusyTimeoutStore opens its own

	store := lowBusyTimeoutStore(t, dataDir)

	delivered := make(chan AsyncCompletion, 1)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	l.store = store

	_, existing, err := l.Start("owner-1", "call-1", "echo hi", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)
	l.acknowledged(jobOf(l, "owner-1", "call-1"))

	busyDone := make(chan struct{})
	go func() {
		defer close(busyDone)
		holdWriteLock(t, dataDir, 250*time.Millisecond)
	}()
	time.Sleep(30 * time.Millisecond) // let the holder actually acquire the write lock first

	start := time.Now()
	l.finish(jobOf(l, "owner-1", "call-1"), jobResult{content: "ok"})
	elapsed := time.Since(start)
	<-busyDone

	got := drainCompletions(delivered)
	require.Len(t, got, 1, "the transition must eventually commit and deliver exactly once")
	require.Equal(t, "ok", got[0].Content)
	require.Greater(t, elapsed, 100*time.Millisecond, "must have actually backed off and retried across the busy window, not returned instantly")
}

// TestWorkLedger_InFlightLatchSkipsSecondTriggerWithoutBlocking pins the
// in-flight latch (doc sec.3.1): while transition is retrying for a job
// (the busy-DB window above), a SECOND cause arriving for the SAME job
// returns immediately (SKIP) rather than blocking behind the first.
//
// Revert check performed: temporarily removed the `job.transitioning`
// short-circuit from commitTransition's guard clause -- this test FAILED
// (the second call's elapsed time matched the first's ~250ms+ retry
// window instead of returning near-instantly, because it entered its own
// retry loop against the same busy DB). Restored the guard; re-ran, passed.
// Diffed work_ledger_transition.go against git HEAD: no diff.
func TestWorkLedger_InFlightLatchSkipsSecondTriggerWithoutBlocking(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	require.NoError(t, db.Release(dataDir))

	store := lowBusyTimeoutStore(t, dataDir)
	delivered := make(chan AsyncCompletion, 1)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	l.store = store

	_, _, err = l.Start("owner-1", "call-1", "echo hi", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged(jobOf(l, "owner-1", "call-1"))

	busyDone := make(chan struct{})
	go func() {
		defer close(busyDone)
		holdWriteLock(t, dataDir, 300*time.Millisecond)
	}()
	time.Sleep(30 * time.Millisecond)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		l.finish(jobOf(l, "owner-1", "call-1"), jobResult{content: "first"})
	}()
	time.Sleep(30 * time.Millisecond) // let the first call actually start retrying

	secondStart := time.Now()
	l.transition(jobOf(l, "owner-1", "call-1"), causeJobKill, jobResult{content: "second"})
	secondElapsed := time.Since(secondStart)

	<-firstDone
	<-busyDone

	require.Less(t, secondElapsed, 20*time.Millisecond, "a second trigger while the first is retrying must SKIP immediately, not wait")
	got := drainCompletions(delivered)
	require.Len(t, got, 1, "exactly one cause must ever be written/delivered")
	require.Equal(t, "first", got[0].Content, "the SKIPPED second cause must never overwrite the in-flight first one")
}

// TestWorkLedger_TerminalCausesRecordOwnStateNoticeKindAndWake is DUR-1c:
// cancel / timeout / job_kill each record their own state + notice_kind +
// wake in the row, table-driven, and the executor's later finish() call
// (simulating the killed/cancelled/timed-out process's own eventual return)
// does not overwrite the recorded cause.
func TestWorkLedger_TerminalCausesRecordOwnStateNoticeKindAndWake(t *testing.T) {
	cases := []struct {
		name           string
		act            func(l *workLedger)
		wantState      string
		wantNoticeKind string
		wantWake       int64
	}{
		{
			name: "job_kill via StopRunCommandJob",
			act: func(l *workLedger) {
				_, _, err := l.StopRunCommandJob("owner-1", "call-1")
				require.NoError(t, err)
			},
			wantState: "cancelled", wantNoticeKind: "job_kill", wantWake: 0,
		},
		{
			name: "timeout via handleTimeout terminate_and_wake",
			act: func(l *workLedger) {
				l.mu.Lock()
				job := l.bySession["owner-1"].jobs["call-1"]
				job.deadline = time.Now().Add(-time.Second)
				job.timeoutKind = timeoutTerminateAndWake
				l.mu.Unlock()
				l.handleTimeout(job)
			},
			wantState: "timed_out", wantNoticeKind: "timeout_terminated", wantWake: 1,
		},
		{
			name: "session cancel via cancelSession",
			act: func(l *workLedger) {
				l.cancelSession("owner-1")
			},
			wantState: "cancelled", wantNoticeKind: "session_cancel", wantWake: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestAsyncJobStore(t)
			l := newWorkLedger(nil)
			l.store = store
			_, _, err := l.Start("owner-1", "call-1", "sleep 100", "run_command", "", false, false, nil, func() {})
			require.NoError(t, err)
			l.acknowledged(jobOf(l, "owner-1", "call-1"))

			tc.act(l)

			row, err := store.Get(context.Background(), "owner-1", "call-1")
			require.NoError(t, err)
			require.Equal(t, tc.wantState, row.State)
			require.Equal(t, tc.wantNoticeKind, row.NoticeKind)
			require.EqualValues(t, tc.wantWake, row.Wake)
			updatedAt := row.UpdatedAt

			// The executor's later, ordinary finish() call must be a no-op:
			// the job is already terminal, so it must not overwrite the
			// recorded cause (state/notice_kind/updated_at unchanged).
			l.finish(jobOf(l, "owner-1", "call-1"), jobResult{content: "whatever the killed process returned", isError: true})
			row2, err := store.Get(context.Background(), "owner-1", "call-1")
			require.NoError(t, err)
			require.Equal(t, tc.wantState, row2.State)
			require.Equal(t, tc.wantNoticeKind, row2.NoticeKind)
			require.Equal(t, updatedAt, row2.UpdatedAt, "finish's no-op must not touch the row at all")
		})
	}
}

// TestWorkLedger_ShutdownCausedCancellationLeavesRowRunningWritesNoNotice is
// half of DUR-1d: a transition attempt whose executor was cancelled by
// close() itself is suppressed entirely -- no DB write, row stays
// 'running' for the next host to recover, and no notice is delivered.
//
// Revert check performed: temporarily changed the shutdown check in
// commitTransition from `l.closed && job.shutdownCancelled` to always
// `false` -- this test FAILED (the row committed to 'failed' and a
// completion was delivered). Restored the check; re-ran, passed. Diffed
// work_ledger_transition.go against git HEAD: no diff.
func TestWorkLedger_ShutdownCausedCancellationLeavesRowRunningWritesNoNotice(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	delivered := make(chan AsyncCompletion, 1)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	l.store = store

	cancelled := false
	_, _, err := l.Start("owner-1", "call-1", "sleep 100", "bash", "", false, false, nil, func() { cancelled = true })
	require.NoError(t, err)
	l.acknowledged(jobOf(l, "owner-1", "call-1"))

	l.close() // sets closed + job.shutdownCancelled, cancels the executor
	require.True(t, cancelled)

	// Simulate the executor's own delayed completion arriving AFTER
	// shutdown flagged this job (asyncTool.run's finalize, running on its
	// own goroutine, calling finish once ctx.Done() unwinds it).
	l.finish(jobOf(l, "owner-1", "call-1"), jobResult{content: "context canceled", isError: true})

	select {
	case c := <-delivered:
		t.Fatalf("a shutdown-caused transition must not deliver a notice, got %+v", c)
	default:
	}
	row, err := store.Get(context.Background(), "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "the row must stay 'running' for the next host to recover")
}

// TestWorkLedger_NaturalCompletionBeforeCloseIsNotSuppressed is the other
// half of DUR-1d: a natural completion that already committed before
// close() runs must be left alone -- close() must not retroactively affect
// it.
func TestWorkLedger_NaturalCompletionBeforeCloseIsNotSuppressed(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	delivered := make(chan AsyncCompletion, 1)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	l.store = store

	_, _, err := l.Start("owner-1", "call-1", "echo hi", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged(jobOf(l, "owner-1", "call-1"))

	l.finish(jobOf(l, "owner-1", "call-1"), jobResult{content: "done"})
	got := drainCompletions(delivered)
	require.Len(t, got, 1)
	require.Equal(t, "done", got[0].Content)

	row, err := store.Get(context.Background(), "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "completed", row.State)

	// close() running afterward must not touch the already-terminal row.
	l.close()
	row2, err := store.Get(context.Background(), "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, row.State, row2.State)
	require.Equal(t, row.UpdatedAt, row2.UpdatedAt)
}

// TestWorkLedger_StartFailsClosedWhenStoreUnavailable is DUR-8: an
// unavailable/erroring store makes a non-sync Start fail, and no in-memory
// job is registered for it.
//
// Review finding: this test's own local var used to be named
// "executorStarted", but Start itself never calls the cancel func on ANY
// path -- cancel is only ever stored on the job struct for a LATER caller
// (close()/MarkJobStopped/etc.) to invoke, so that assertion could never
// fail regardless of whether fail-closed actually worked (vacuous). Renamed
// to cancelCalled to stop it from misrepresenting what it proves. The REAL
// "the executor (asyncTool's `go t.run`) never runs when Start fails closed"
// property is proven at the layer that owns it:
// TestAsyncTool_InnerNeverRunsWhenStartFailsClosed (async_tool_test.go),
// which observes the INNER tool never running.
func TestWorkLedger_StartFailsClosedWhenStoreUnavailable(t *testing.T) {
	t.Parallel()
	// nil store is DUR-8's literal "no store wired" fail-closed case; a
	// truly broken *sql.DB is covered indirectly by every store-level error
	// path (internal/session/async_job_store_test.go) already returning a
	// non-nil error that Start propagates verbatim.
	l := newWorkLedger(nil)
	l.store = nil

	cancelCalled := false
	_, _, err := l.Start("owner-1", "call-1", "echo hi", "bash", "", false, false, nil, func() { cancelCalled = true })
	require.Error(t, err)
	require.False(t, cancelCalled, "Start itself never invokes the cancel func on any path; a failed-closed Start must not either")

	l.mu.Lock()
	_, present := l.bySession["owner-1"]
	l.mu.Unlock()
	if present {
		require.Empty(t, l.bySession["owner-1"].jobs, "no in-memory job may exist for a failed-closed Start")
	}
}

// TestWorkLedger_StartSyncJobWorksWithoutStore pins the other half of DUR-8:
// a sync (SDK-origin) job never touches the store at all, so it must still
// work even with none wired.
func TestWorkLedger_StartSyncJobWorksWithoutStore(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = nil

	job, existing, err := l.Start("owner-1", "call-1", "echo hi", "bash", "", false, true, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)
	require.NotNil(t, job)

	l.finish(jobOf(l, "owner-1", "call-1"), jobResult{content: "ok"})
	res, err := l.awaitSync(context.Background(), job)
	require.NoError(t, err)
	require.Equal(t, "ok", res.content)
}

// TestWorkLedger_StartClaimIdempotencyThroughLedger complements the store-
// level idempotency test: a repeated Start for the SAME (owner, tool_call_id,
// input) through the LEDGER returns the existing in-memory job, and the
// durable row is claimed exactly once (Claim's own idempotent-read branch,
// not a second insert).
func TestWorkLedger_StartClaimIdempotencyThroughLedger(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	l := newWorkLedger(nil)
	l.store = store

	job1, existing1, err := l.Start("owner-1", "call-1", "echo hi", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing1)

	job2, existing2, err := l.Start("owner-1", "call-1", "echo hi", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.True(t, existing2, "a repeated call with the SAME input must be reported existing")
	require.Same(t, job1, job2, "must return the SAME in-memory job, not a duplicate")
}

// TestWorkLedger_StartASYNC01ConflictRefusedBeforeStarted pins ASYNC-01
// through the ledger: a delegation naming a child session id a RUNNING row
// already claims is refused before the tool would ever report "started",
// with a clear, model-facing message.
func TestWorkLedger_StartASYNC01ConflictRefusedBeforeStarted(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	l := newWorkLedger(nil)
	l.store = store

	_, _, err := l.Start("parent-1", "call-1", "do work", AgentToolName, "child-1", false, false, nil, func() {})
	require.NoError(t, err)

	_, _, err = l.Start("parent-2", "call-2", "do other work", AgentToolName, "child-1", false, false, nil, func() {})
	require.Error(t, err)
	require.Contains(t, err.Error(), "wait for its result")

	l.mu.Lock()
	_, present := l.bySession["parent-2"]
	l.mu.Unlock()
	if present {
		require.Empty(t, l.bySession["parent-2"].jobs)
	}
}
