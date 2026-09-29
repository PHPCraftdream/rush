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
	l.acknowledged("owner-1", "call-1")

	// Simulate "App shutdown closes the DB" (review Scenario A): every
	// subsequent store call now fails immediately (sql: database is
	// closed), not blocks -- exactly what drives commitTransition into its
	// retry loop.
	require.NoError(t, conn.Close())

	done := make(chan struct{})
	go func() {
		defer close(done)
		l.finish("owner-1", "call-1", jobResult{content: "ok"})
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
		l.acknowledged("owner", "call")

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			l.finish("owner", "call", jobResult{content: "ok"})
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
	l.setRunCommandBuffer("owner", "call", buf)

	_, stopErr := l.StopRunCommandJob("owner", "call")
	require.NoError(t, stopErr)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := l.awaitSync(ctx, job)
	require.NoError(t, err, "a sync StopRunCommandJob must still unblock awaitSync with a real outcome, not hang")
	require.False(t, res.isError)
	require.Equal(t, "partial output\n", res.content, "must use the live buffer snapshot")
}
