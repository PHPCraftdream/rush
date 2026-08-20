package session_test

// Regression coverage for task #607/P0: the SIXTH fix to DrainSessionNow's
// "never report a completed drain over outstanding work" contract, and the
// only one of the six caught before shipping.
//
// Five prior task reviews (#575, #578, #588, #592, #593) each closed one
// specific shape of the same defect -- a DrainSessionNow return value that
// misrepresents "this call's OWN ctx ended with genuine work left
// unaccounted for" as "the continuation fully completed" (DrainComplete,
// nil), the ONLY pairing app_run.go's RunNonInteractive converts directly
// into exit code 0. This file pins the shape those five reviews left open:
// the loop's OWN two ctx-liveness checks (top-of-loop, and the
// admission-wait `case <-ctx.Done()`) computed their verdict via a bare
// `ledger.verdict(false)` that trusted the ledger's PRE-EXISTING state
// (whatever an earlier row in the SAME call already recorded) to explain
// why the call is stopping here -- without ever recording that THIS
// specific exit is happening because ctx died. If an earlier row in the
// same call had already recordSuccess'd and nothing else had failed, that
// bare verdict() call returned (DrainComplete, nil) even though the
// row that triggered this loop iteration was never even looked at.
//
// Three OTHER early-exit branches in the exact same loop (the lease-error
// path, the execSem-wait ctx.Done() path, and the pump-stopping path)
// already recorded a representative error into the ledger before computing
// their own final verdict -- so the two ctx-liveness checks were the only
// asymmetric branches, not a defect shared by the whole function. See
// run_queue_drain_session.go's rowLedger.verdictOnCtxDone doc for the fix:
// a single function both ctx-liveness checks now route through, which
// cannot compute a verdict without first unconditionally folding ctx.Err()
// into the ledger.
//
// This test drives the TOP-OF-LOOP branch specifically (not the
// admission-wait branch): row A is leased and executed by THIS
// DrainSessionNow call's own local-execution loop, succeeds, and the
// coordinator cancels the caller's ctx as a side effect of that same
// return -- so the very next spin through the `for` loop hits `if
// ctx.Err() != nil` at the top with row A already recorded as a success
// and row B never even leased.

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// successThenCancelCoordinator reports a clean success for its first
// Coordinator.Run call (row A) and cancels the caller-supplied cancel func
// as a side effect of that SAME call, before returning -- deterministically
// forcing DrainSessionNow's NEXT loop iteration to observe ctx.Err() != nil
// at the top of the loop with row A already resolved. Row B must never be
// leased: the top-of-loop check runs before any lease attempt.
type successThenCancelCoordinator struct {
	calls  int
	cancel context.CancelFunc
}

func (c *successThenCancelCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls++
	if c.calls == 1 {
		c.cancel()
		// Give the cancellation a moment to be observable to anything
		// racing it (not required for correctness -- ctx.Err() is set
		// synchronously by cancel() -- but avoids any doubt about ordering
		// in a slow CI environment).
		time.Sleep(5 * time.Millisecond)
		return nil, nil
	}
	// Must never be reached: the top-of-loop ctx check must stop this call
	// before row B is ever leased.
	return nil, nil
}

// TestDrainSessionNow_CtxDiesAfterOneSuccess_NeverReportsCleanSuccess pins
// the exact scenario task #607 reproduced: one row committed cleanly, the
// call's own ctx then ends, and a second row is left pending, never looked
// at. The call must NOT return (DrainComplete, nil) -- the only pairing a
// caller may read as "the continuation fully completed" and use to
// override an original cancellation with success (see app_run.go's
// drainOutcomeError and DrainSessionNow's own doc).
//
// Revert-check (see task report for verbatim command output): reverting
// JUST the top-of-loop `return ledger.verdictOnCtxDone(ctx)` back to the
// pre-fix `result, verdictErr := ledger.verdict(false); if result ==
// DrainNoWork { return DrainNoWork, ctx.Err() }; return result, verdictErr`
// reproduces (DrainComplete, nil) and fails this test, while leaving the
// admission-wait branch's OWN fix in place -- proving this test covers the
// top-of-loop site specifically, not the admission-wait site's equivalent
// fix (see the sibling test below for that one).
func TestDrainSessionNow_CtxDiesAfterOneSuccess_NeverReportsCleanSuccess(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p607-ctx-dies-after-success")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coord := &successThenCancelCoordinator{cancel: cancel}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p607-ctx-dies",
	})
	enqueueTwoRows(t, "p607-ctx-dies", svc, sess.ID)

	result, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.Equal(t, 1, coord.calls, "row A must have executed exactly once; row B must NEVER be leased once ctx has died")
	require.False(t, result == session.DrainComplete && drainErr == nil,
		"MUST NOT report (DrainComplete, nil): row A succeeded but ctx died before row B (still pending) was ever looked at -- app_run.go's drainOutcomeError converts exactly this pairing into exit code 0")
	require.Error(t, drainErr, "the call stopped because its OWN ctx ended with a row still outstanding -- that must surface as an error, not silence")
	require.ErrorIs(t, drainErr, context.Canceled, "the representative error must trace back to this call's own ctx cancellation")

	// Row B must remain durably pending, untouched -- it was never leased.
	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	var sessionPending []session.RunQueueEntry
	for _, e := range pending {
		if e.SessionID == sess.ID {
			sessionPending = append(sessionPending, e)
		}
	}
	require.Len(t, sessionPending, 1, "exactly row B must still be pending for this session")
	require.Contains(t, sessionPending[0].CallData, "row-B", "the still-pending row must be row B, never touched")
	require.Equal(t, int64(0), sessionPending[0].Attempts, "row B was never leased/attempted -- no attempt penalty")
}
