// Coverage for the Ф4-2 orchestrator review's P1/P2 findings: retry loops
// must stop once the ledger is closed (P1), Stop racing a natural finish
// must never wake the session (P2), and a sync job stopped via job_kill
// must keep its "stopped" outcome (P2, regression of #1023 for library
// mode).
package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestWorkLedger_RetryLoopStopsAfterClose pins P1: a store-write retry loop
// (here, finish's transition call) must stop promptly once the ledger is
// closed, instead of leaking a goroutine sleeping out the full backoff
// schedule against a DB the process is tearing down (review Scenario A:
// "App shutdown closes the DB ... a leaked goroutine for the rest of the
// process").
//
// Revert-check performed: temporarily replaced commitTransition's
// interruptible `select { case <-l.closedCh: ...; case <-time.After(...): }`
// with a plain `time.Sleep(asyncStoreRetryBackoff(attempt))` -- this test
// FAILED (the goroutine was still sleeping ~150-300ms after close() when
// the 100ms post-close assertion window elapsed). Restored the select;
// re-ran, passed. Diffed work_ledger_transition.go against git HEAD after
// restoring: matches the committed fix.
func TestWorkLedger_RetryLoopStopsAfterClose(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	store := session.NewAsyncJobStore(conn, dataDir, 1, "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	l := newWorkLedger(nil)
	l.store = store

	_, _, err = l.Start("owner-1", "call-1", "echo hi", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged(jobOf(l, "owner-1", "call-1"))

	// Simulate "App shutdown closes the DB" (review Scenario A): every
	// subsequent store call now fails immediately (sql: database is
	// closed), not blocks -- exactly what drives commitTransition into its
	// retry loop.
	require.NoError(t, conn.Close())

	done := make(chan struct{})
	go func() {
		defer close(done)
		l.finish(jobOf(l, "owner-1", "call-1"), jobResult{content: "ok"})
	}()

	// Let several failed attempts accumulate (10+20+40+80ms ~= 150ms) so the
	// goroutine is parked in a backoff wait comfortably longer than the
	// post-close assertion window below -- otherwise a tiny current backoff
	// could mask a non-interruptible Sleep as "prompt" by accident.
	time.Sleep(150 * time.Millisecond)
	closeStart := time.Now()
	l.close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("finish's retry loop did not return at all after close()")
	}
	require.Less(t, time.Since(closeStart), 100*time.Millisecond,
		"the retry loop must return promptly after close(), not after sleeping out its current backoff window")
}

// TestWorkLedger_CancelSessionRaceAgainstNaturalFinishNeverWakes pins P2: a
// plain (non-delegation) job's natural finish racing cancelSession's Stop
// must never wake the session, regardless of which cause's DB write
// actually wins the CAS -- cancelSession marks the job stoppedBySession
// under l.mu, synchronously with every other target, BEFORE either side's
// DB write begins, so transition's delivery step can drop it silently no
// matter who commits first.
//
// The in-memory onWebDone callback never firing is necessary but not
// sufficient: causeStateNoticeKindWake sets wake=true for causeNaturalFinish
// UNCONDITIONALLY, with no knowledge of stoppedBySession at all (that gate is
// purely in-memory, checked only by deliverLocked, after the DB write
// already committed) -- a bug that made the DB row ITSELF carry a stray
// wake=1/pending row despite session_cancel actually winning would slip past
// a memory-only check. This asserts the COMMITTED row is coherent with
// whichever cause actually won: natural finish winning legitimately leaves
// wake=1/pending (real, future debt for the next ordinary turn -- only the
// immediate HINT is suppressed, not the row); session_cancel winning leaves
// wake=0/session_cancel (no debt at all).
//
// Revert-check performed: temporarily removed the `job.stoppedBySession &&
// job.childSession == ""` branch from transition's delivery step (falling
// through to the unconditional deliverLocked call) -- this test FAILED
// (some iterations delivered a completion, since finish's own transition
// call sometimes won the race and reached deliverLocked normally). Restored
// the branch; re-ran 30 iterations, passed. Diffed work_ledger_transition.go
// against git HEAD after restoring: matches the committed fix.
func TestWorkLedger_CancelSessionRaceAgainstNaturalFinishNeverWakes(t *testing.T) {
	for i := 0; i < 30; i++ {
		store := newTestAsyncJobStore(t)
		delivered := make(chan AsyncCompletion, 4)
		l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
		l.store = store

		_, _, err := l.Start("owner", "call", "", "bash", "", false, false, nil, func() {})
		require.NoError(t, err)
		l.acknowledged(jobOf(l, "owner", "call"))

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			l.finish(jobOf(l, "owner", "call"), jobResult{content: "ok"})
		}()
		go func() {
			defer wg.Done()
			<-start
			l.cancelSession("owner")
		}()
		close(start)
		wg.Wait()

		got := drainCompletions(delivered)
		require.Empty(t, got, "a plain job must never wake the session after Stop, regardless of which cause won the race (iteration %d)", i)

		row, err := store.Get(context.Background(), "owner", "call")
		require.NoError(t, err, "iteration %d", i)
		switch row.State {
		case "completed":
			require.EqualValues(t, 1, row.Wake, "iteration %d: natural finish winning must still leave real, future debt in the row", i)
			require.Equal(t, "ok", row.ResultSummary.String, "iteration %d", i)
		case "cancelled":
			require.EqualValues(t, 0, row.Wake, "iteration %d: session_cancel winning must never leave wake=1", i)
			require.Equal(t, "session_cancel", row.NoticeKind, "iteration %d", i)
		default:
			t.Fatalf("iteration %d: unexpected committed state %q", i, row.State)
		}
	}
}

// TestWorkLedger_MarkJobStopped_SyncJobPreservesStoppedOutcome pins the P2
// sync job_kill regression: a sync (SDK-origin) job stopped via job_kill
// must still reach a well-formed "stopped (job_kill)" outcome (Stopped
// semantics: phaseCancelled, not an error) through the OLD memory-only path
// -- sync jobs never touch the store (doc sec.3.1), and l.transition alone
// would silently no-op for one (commitTransition skips sync jobs), leaving
// a blocked awaitSync caller hanging forever with no outcome at all.
//
// Revert-check performed: temporarily made MarkJobStopped call
// l.transition unconditionally (dropping the sync branch) -- this test
// FAILED with a context.DeadlineExceeded error (awaitSync never unblocked,
// since transition silently no-ops for a sync job and job.done is never
// closed). Restored the sync branch; re-ran, passed.
func TestWorkLedger_MarkJobStopped_SyncJobPreservesStoppedOutcome(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil) // sync path never touches the store
	job, existing, err := l.Start("owner", "call", "", "bash", "", false, true, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)

	l.MarkJobStopped("owner", "call")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := l.awaitSync(ctx, job)
	require.NoError(t, err, "a sync job_kill must still unblock awaitSync with a real outcome, not hang")
	require.False(t, res.isError, "a job_kill stop is not a failure")
	require.Contains(t, res.content, "still running", "capturePartial's fallback placeholder (no l.coord/background wired in this isolated test)")
}

// TestWorkLedger_StopRunCommandJob_SyncJobPreservesStoppedOutcome is the
// StopRunCommandJob counterpart of the MarkJobStopped test above.
func TestWorkLedger_StopRunCommandJob_SyncJobPreservesStoppedOutcome(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	buf := &fakeLiveOutputBuffer{}
	buf.write("partial output\n")
	job, existing, err := l.Start("owner", "call", "", "run_command", "", false, true, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)
	l.setRunCommandBuffer(jobOf(l, "owner", "call"), buf)

	_, _, stopErr := l.StopRunCommandJob("owner", "call")
	require.NoError(t, stopErr)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := l.awaitSync(ctx, job)
	require.NoError(t, err, "a sync StopRunCommandJob must still unblock awaitSync with a real outcome, not hang")
	require.False(t, res.isError)
	require.Equal(t, "partial output\n", res.content, "must use the live buffer snapshot")
}

// TestWorkLedger_StopBeforeAckDoesNotOrphanRow pins B10: Stop landing between
// Claim and the "started" tool-result ack must not drop the in-memory job
// before announced ever flips. The pre-fix delivery step dropped a
// stoppedBySession-marked plain job unconditionally, the instant its cause's
// CAS committed -- regardless of job.announced. If that happens BEFORE the
// ack, the eventual acknowledgeWithMessageTx/acknowledged call finds no job
// left in s.jobs, so MarkAnnounced never even gets attempted from the
// ledger's own follow-up (finishAcknowledgeLocally no-ops on a nil job) --
// the DB row is then stuck at announced=0/delivery=pending forever: never a
// pull candidate (requires announced=1), never purged (retention excludes
// 'running'... no, this row IS terminal ('cancelled') but announced=0 is not
// checked by PurgeAsyncJobsOlderThan either), an orphan.
//
// Revert-check performed: reverted deliverLocked's stoppedBySession branch to
// its pre-fix position (back in transition()/commitAndDeliver, unconditional
// on the CAS commit rather than gated by deliverLocked's own !job.announced
// top guard) -- this test FAILED (stillPresent was false right after
// cancelSession, and the later acknowledged() call left row.Announced at 0).
// Restored the fix (the drop now lives inside deliverLocked, reached only
// once announced is actually true); re-ran, passed. Diffed work_ledger.go
// against git HEAD after restoring: matches the committed fix.
func TestWorkLedger_StopBeforeAckDoesNotOrphanRow(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	delivered := make(chan AsyncCompletion, 4)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	l.store = store

	_, _, err := l.Start("owner", "call", "", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	// Deliberately no l.acknowledged call yet -- Stop races ahead of the
	// "started" tool-result write (B10's exact window).

	l.cancelSession("owner")

	l.mu.Lock()
	_, stillPresent := l.bySession["owner"].jobs["call"]
	l.mu.Unlock()
	require.True(t, stillPresent, "an unannounced job must survive Stop in memory so the eventual ack can still close it out")

	row, err := store.Get(context.Background(), "owner", "call")
	require.NoError(t, err)
	require.Equal(t, "cancelled", row.State)
	require.EqualValues(t, 0, row.Announced, "not yet announced at this point")

	// The "started" ack finally arrives.
	l.acknowledged(jobOf(l, "owner", "call"))

	row, err = store.Get(context.Background(), "owner", "call")
	require.NoError(t, err)
	require.EqualValues(t, 1, row.Announced, "the ack must still be able to mark this row announced -- B10's fix")

	l.mu.Lock()
	jobs := l.bySession["owner"].jobs
	l.mu.Unlock()
	require.Empty(t, jobs, "the job must be dropped from memory once announced -- still no in-memory notice for a plain Stop")

	require.Empty(t, drainCompletions(delivered), "a plain job stopped by Stop must never produce an in-memory notice, even discovered late at ack")
}

// TestWorkLedger_CloseDoesNotLatchOntoExecutorReturnedJob pins B9: close()
// must not latch shutdownCancelled onto a job whose executor has ALREADY
// returned with a real result and is merely waiting its turn for l.mu to
// report it. finish() sets job.executorReturned the INSTANT it acquires
// l.mu, before any DB work -- this reproduces the exact window that flag
// protects: executorReturned is set (finish()'s own first action), THEN
// close() runs (which, pre-fix, marked shutdownCancelled on every non-
// terminal job unconditionally), THEN the natural finish's own transition
// call finally proceeds. The real result must still commit -- not be
// silently discarded, leaving the row 'running' for recovery to later
// misreport as 'interrupted'.
//
// Revert-check performed: reverted close() to its pre-fix body (mark
// shutdownCancelled on every job present, no state/executorReturned check)
// -- this test FAILED (0 completions delivered, row.State stayed "running").
// Restored the fix; re-ran, passed. Diffed work_ledger.go against git HEAD
// after restoring: matches the committed fix.
func TestWorkLedger_CloseDoesNotLatchOntoExecutorReturnedJob(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	delivered := make(chan AsyncCompletion, 1)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	l.store = store

	_, _, err := l.Start("owner", "call", "", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged(jobOf(l, "owner", "call"))

	// Simulate finish()'s own ordering: it marks executorReturned BEFORE
	// doing any DB work. Reproduces the window where the executor's real
	// result is already known but not yet committed when close() runs.
	l.mu.Lock()
	job := l.bySession["owner"].jobs["call"]
	job.executorReturned = true
	l.mu.Unlock()

	l.close()

	// The natural finish's own (unrelated-to-shutdown) transition call now
	// proceeds -- must still commit the real result.
	l.transition(jobOf(l, "owner", "call"), causeNaturalFinish, jobResult{content: "real output"})

	got := drainCompletions(delivered)
	require.Len(t, got, 1, "a natural completion racing close() must still be delivered, not silently dropped")
	require.Equal(t, "real output", got[0].Content)
	require.False(t, got[0].IsError)

	row, err := store.Get(context.Background(), "owner", "call")
	require.NoError(t, err)
	require.Equal(t, "completed", row.State, "must not be left 'running' -- its real output was already known, not actually shutdown-caused")
}
