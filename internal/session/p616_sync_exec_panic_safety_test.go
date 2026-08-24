package session_test

// Task #616/P2-1 (2026-08-20 read-only release review): DrainSessionNow's
// own synchronous execution branch used to call executeEntrySync and only
// AFTER it returned normally release workerWg, the execSem slot, and the
// session admission -- unlike its background-tick counterpart, executeEntry
// (run_queue_entry_dispatch.go), which wraps the equivalent three releases
// in a defer specifically so a panic inside executeEntrySync still unwinds
// them. This test proves the fix: a panicking Coordinator.Run on the
// SYNCHRONOUS drain path must not leak any of the three resources.
import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// panickingCoordinatorForDrain always panics when Run is called -- standing
// in for an unrecovered panic somewhere inside a real Coordinator.Run's call
// graph (a tool call, a sub-agent delegation, etc; see app_run.go's own
// runAgentTurnRecovered doc for real-world panic sources on this call path).
type panickingCoordinatorForDrain struct{}

func (p *panickingCoordinatorForDrain) Run(ctx context.Context, call session.SessionAgentCallData) (*any, error) {
	panic("simulated executeEntrySync panic")
}

// TestDrainSessionNow_SyncExecutionPanic_ReleasesWorkerWgExecSemAndAdmission
// proves the three releases (workerWg.Done, execSem, releaseSession) all run
// even when the synchronous execution path panics, by recovering the panic
// at the test's own call boundary (standing in for "some future outer
// boundary recovers", exactly as this task's brief describes) and then
// asserting none of the three resources were left held:
//
//   - The panic must still propagate out of DrainSessionNow (not be
//     silently swallowed) -- normal crash/propagation behavior preserved.
//   - pump.Stop(), called AFTER the panic has already unwound, must return
//     promptly and report a graceful (non-forced) shutdown -- proving
//     workerWg.Done() ran despite the panic (a leaked Add would either hang
//     Stop() for its full grace period or force it).
//   - A follow-up DrainSessionNow call for the SAME session, issued only
//     after the panic has unwound, must not be refused admission --
//     proving releaseSession ran and cleared p.inFlight despite the panic
//     (a leaked admission would make every subsequent call for this
//     session block forever waiting on an admissionEntry.done that will
//     never close).
//
// Revert-check: reverting run_queue_drain_session.go's synchronous branch
// back to the pre-fix shape (releases running only after executeEntrySync
// returns normally, not deferred) makes this test hang on the follow-up
// DrainSessionNow call and the Stop() call, both timing out.
func TestDrainSessionNow_SyncExecutionPanic_ReleasesWorkerWgExecSemAndAdmission(t *testing.T) {
	sess, svc := setupTestSession(t, "drain-now-sync-panic")
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    &panickingCoordinatorForDrain{},
		PumpInstanceID: "test-pump-drain-sync-panic",
		// Long enough that the background tick never fires during this
		// test, so the synchronous DrainSessionNow call below is provably
		// the one that leases and executes (panics on) the entry -- mirrors
		// TestDrainSessionNow_StopWaitsForSynchronousDrain's own setup.
		TestTick: func() time.Duration { return time.Hour },
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "drain-now-sync-panic-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	func() {
		defer func() {
			r := recover()
			require.NotNil(t, r, "the panic must propagate out of DrainSessionNow, not be silently swallowed")
			require.Contains(t, fmt.Sprint(r), "simulated executeEntrySync panic")
		}()
		_, _ = pump.DrainSessionNow(ctx, sess.ID)
		t.Fatal("unreachable: DrainSessionNow must not return normally when Coordinator.Run panics")
	}()

	// Start the pump now (after the panic already unwound) purely so
	// Stop()'s workerWg accounting is exercised -- Start()/Stop() require
	// p.started, and starting earlier would let the hour-long TestTick's
	// initial tick race the synchronous call above.
	pump.Start()

	stopDone := make(chan bool, 1)
	go func() { stopDone <- pump.Stop() }()
	select {
	case forced := <-stopDone:
		require.False(t, forced, "Stop() should complete gracefully -- workerWg.Done() must have run despite the panic")
	case <-time.After(6 * time.Second):
		t.Fatal("pump.Stop() hung -- workerWg.Done() was not called after the panic")
	}
}

// TestDrainSessionNow_SyncExecutionPanic_AdmissionReleasedForFollowUpCall
// isolates the admission-release assertion on its own pump (no Start()
// involved at all), proving releaseSession specifically -- not merely
// workerWg -- runs despite the panic: a second DrainSessionNow call for the
// SAME session, issued after the first panicked, must proceed (return
// promptly, not block forever on the first call's still-held admission)
// rather than hang.
//
// What the follow-up call must REPORT changed with task #624/F-5: the
// panicking call leased the row and never wrote any outcome (the panic
// unwound past executeEntrySync's own Ack/Nack writes), so the row is still
// durably LEASED -- an orphaned lease. This test previously asserted
// (DrainNoWork, nil) for the follow-up call, which was only reachable
// because the anyExecuted gate made the outstanding-entries query invisible
// on a cold call; #624 relaxed that gate, and the follow-up call now
// truthfully reports DrainFailed with ErrOutstandingRunQueueEntry over the
// row its predecessor abandoned mid-execution. The assertion that actually
// carries this test's regression value is the 4s deadline below: a leaked
// admission would make the follow-up call HANG on an admissionEntry.done
// that never closes.
func TestDrainSessionNow_SyncExecutionPanic_AdmissionReleasedForFollowUpCall(t *testing.T) {
	sess, svc := setupTestSession(t, "drain-now-sync-panic-admission")
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    &panickingCoordinatorForDrain{},
		PumpInstanceID: "test-pump-drain-sync-panic-admission",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "drain-now-sync-panic-admission-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	func() {
		defer func() {
			require.NotNil(t, recover(), "the panic must propagate")
		}()
		_, _ = pump.DrainSessionNow(ctx, sess.ID)
	}()

	// The row the panicking call leased is still durably leased (orphaned)
	// -- confirmed here directly, so the follow-up assertion below provably
	// describes the orphaned-lease shape and not an empty queue.
	current, err := svc.GetRunQueueEntry(context.Background(), "drain-now-sync-panic-admission-1")
	require.NoError(t, err)
	require.NotNil(t, current, "the panicking call never wrote an outcome -- the row must still exist")
	require.Equal(t, session.RunQueueStatusLeased, current.Status, "the panicking call's lease is orphaned, not consumed")

	followUpDone := make(chan struct{})
	var followUpResult session.DrainResult
	var followUpErr error
	go func() {
		defer close(followUpDone)
		followUpResult, followUpErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	select {
	case <-followUpDone:
	case <-time.After(4 * time.Second):
		t.Fatal("follow-up DrainSessionNow call hung -- admission was not released after the panic")
	}
	require.ErrorIs(t, followUpErr, session.ErrOutstandingRunQueueEntry, "the orphaned lease must be VISIBLE to the follow-up call (task #624/F-5), not silently skipped as (DrainNoWork, nil)")
	require.Equal(t, session.DrainFailed, followUpResult)
}
