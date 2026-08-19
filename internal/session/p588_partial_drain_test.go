package session_test

// Regression coverage for task #588/P0-2 of the 2026-08-19 release-readiness
// follow-up review: classifyBackgroundOutcome's busy/queued/foreign-lock
// outcome (stopNow=true) used to make BOTH of DrainSessionNow's stopNow
// return points hand back `drained, nil` unconditionally -- discarding
// whatever err an EARLIER row in the same call had already accumulated,
// and, even with no prior error, mislabelling "one row ran, then the next
// hit a busy/contended session" as complete success.
//
// app_run.go's RunNonInteractive converts (drained=true, err=nil) directly
// into exit code 0 with a success envelope. With stacked durable rows, an
// OS session lock or in-process mailbox owner can legitimately change hands
// BETWEEN two rows processed by the SAME DrainSessionNow call -- this is
// not hypothetical, see this file's header in
// run_queue_drain_session.go's own doc and the review itself.
//
// Each test below enqueues TWO rows (A, B) for the same session and forces
// row A to resolve with a specific outcome, then row B to report the "busy"
// outcome (ErrCallQueuedNotExecuted) as if a different live owner grabbed
// the session between the two rows. All four pin the same requirement: NO
// combination may produce a successful exit (drained=true, err=nil), and
// row B must remain durably pending afterward (never touched, no attempt
// penalty).

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// twoRowCoordinator lets a test control exactly what each of the first two
// Coordinator.Run calls for a session return, so a stacked-row partial
// drain (row A resolves one way, row B is found busy) can be forced
// deterministically instead of relying on real session-lock/mailbox
// contention timing.
type twoRowCoordinator struct {
	calls   int
	firstFn func() (*any, error)
}

func (c *twoRowCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls++
	if c.calls == 1 {
		return c.firstFn()
	}
	// Every call from the second onward reports busy -- simulating a
	// different live owner having taken the session between row A and row
	// B (or B and any further row).
	return nil, session.ErrCallQueuedNotExecuted
}

// enqueueTwoRows enqueues durable rows A and B (FIFO -- A leases first) for
// a single fresh session and returns everything a test needs to drive and
// assert on the scenario.
func enqueueTwoRows(t *testing.T, name string, sessions session.Service, sessionID string) {
	t.Helper()
	callDataA, err := json.Marshal(map[string]any{"SessionID": sessionID, "Prompt": "row-A"})
	require.NoError(t, err)
	require.NoError(t, sessions.EnqueueRunQueueEntry(context.Background(), name+"-row-A", sessionID, callDataA))

	callDataB, err := json.Marshal(map[string]any{"SessionID": sessionID, "Prompt": "row-B"})
	require.NoError(t, err)
	require.NoError(t, sessions.EnqueueRunQueueEntry(context.Background(), name+"-row-B", sessionID, callDataB))
}

// TestDrainSessionNow_PartialDrain_SuccessThenBusy_NeverReportsCleanSuccess
// pins the review's headline scenario verbatim: row A succeeds cleanly,
// then row B is found busy. The pre-fix code returned (true, nil) here --
// indistinguishable from "the whole continuation finished" -- even though B
// never ran at all.
//
// Revert-check (see task prompt): change the executed-here stopNow branch's
// `if err == nil && drained { err = ErrDrainIncomplete }` back to an
// unconditional `return drained, nil` and this test fails on the
// require.Error below -- exactly the false success task #588 exists to
// close. VERBATIM output captured while validating this is recorded in
// this file's trailing comment block.
func TestDrainSessionNow_PartialDrain_SuccessThenBusy_NeverReportsCleanSuccess(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "p588-success-then-busy")
	coord := &twoRowCoordinator{firstFn: func() (*any, error) { return nil, nil }}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p588-success-busy",
	})
	enqueueTwoRows(t, "p588-success-busy", svc, sess.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drained, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.Equal(t, 2, coord.calls, "both row A and row B's Coordinator.Run must have been attempted")
	require.True(t, drained, "row A genuinely executed and committed")
	require.Error(t, drainErr, "row B was left undrained (busy) after row A succeeded -- this must NOT be reported as a clean success")
	require.ErrorIs(t, drainErr, session.ErrDrainIncomplete, "the specific signal must be ErrDrainIncomplete: a row genuinely ran, but the queue was only partially drained")

	// Row B must remain durably pending, untouched, for the next owner
	// (this process's next call, another process, or the background tick)
	// to pick up -- see the task prompt's "last pending row must remain
	// observable" contract.
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
	require.Equal(t, int64(0), sessionPending[0].Attempts, "row B was never actually leased/attempted by this call -- no attempt penalty")
}

// TestDrainSessionNow_PartialDrain_RetryableErrorThenBusy_PreservesError
// pins the "retryable error -> busy" contract: row A fails with an
// ordinary retryable error (left pending via Nack, attempts incremented),
// and instead of this call re-leasing row A again on its next loop
// iteration, a DIFFERENT owner takes the session (busy). The accumulated
// retryable error must reach the caller -- not be discarded for a bare nil.
func TestDrainSessionNow_PartialDrain_RetryableErrorThenBusy_PreservesError(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "p588-retryable-then-busy")
	retryErr := errors.New("p588: simulated transient retryable failure for row A")
	coord := &twoRowCoordinator{firstFn: func() (*any, error) { return nil, retryErr }}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p588-retryable-busy",
	})
	// A SINGLE row here, deliberately: row A itself fails retryably and
	// stays pending (same row, not a second one) -- the very next loop
	// iteration re-attempts THAT row via LeaseRunQueueEntry, and the
	// coordinator's second call (this time for the SAME row A) reports
	// busy, modelling a different owner grabbing the session between this
	// call's own retry attempts.
	callDataA, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "row-A"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "p588-retryable-busy-row-A", sess.ID, callDataA))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drained, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.Equal(t, 2, coord.calls, "row A must have been attempted once (retryable failure), then re-leased and found busy on retry")
	require.True(t, drained, "row A's first attempt genuinely executed and reached a resolution (the retryable failure)")
	require.Error(t, drainErr, "a retryable failure was accumulated for row A; it must reach the caller, not be silently discarded for a bare nil")
	require.ErrorIs(t, drainErr, retryErr, "the SPECIFIC retryable error must be preserved, not replaced by a generic sentinel")

	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	var sessionPending []session.RunQueueEntry
	for _, e := range pending {
		if e.SessionID == sess.ID {
			sessionPending = append(sessionPending, e)
		}
	}
	require.Len(t, sessionPending, 1, "row A must still be pending (Nacked, not lost) for a later owner to retry")
	require.Equal(t, int64(1), sessionPending[0].Attempts, "exactly one attempt was charged for the one retryable failure")
}

// TestDrainSessionNow_PartialDrain_TerminalErrorThenBusy_PreservesError pins
// the "terminal error -> busy" contract: row A terminal-fails
// (AlreadyAttempted -- permanently deleted, never retried), then row B is
// found busy. The terminal failure must reach the caller.
func TestDrainSessionNow_PartialDrain_TerminalErrorThenBusy_PreservesError(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "p588-terminal-then-busy")
	termErr := &agent.ErrCallAlreadyAttempted{Err: errors.New("p588: row A already attempted")}
	coord := &twoRowCoordinator{firstFn: func() (*any, error) { return nil, termErr }}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p588-terminal-busy",
	})
	enqueueTwoRows(t, "p588-terminal-busy", svc, sess.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drained, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.Equal(t, 2, coord.calls, "row A (terminal failure) and row B (busy) must both have been attempted")
	require.True(t, drained, "row A genuinely executed and reached a resolution (a terminal one)")
	require.Error(t, drainErr, "row A terminal-failed; that permanent loss must reach the caller, not be replaced by a bare nil")
	require.ErrorIs(t, drainErr, termErr, "the SPECIFIC terminal error must be preserved")

	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	var sessionPending []session.RunQueueEntry
	for _, e := range pending {
		if e.SessionID == sess.ID {
			sessionPending = append(sessionPending, e)
		}
	}
	require.Len(t, sessionPending, 1, "row B must still be pending -- row A is gone (terminal-failed), row B was never touched")
	require.Contains(t, sessionPending[0].CallData, "row-B")
	require.Equal(t, int64(0), sessionPending[0].Attempts, "row B was never leased/attempted -- no attempt penalty")
}

// ackFailingServiceP588 wraps a real session.Service and fails
// AckRunQueueEntry for exactly the first Ack it sees (row A's), so row A's
// turn genuinely runs and commits but its terminal bookkeeping write fails
// -- ErrTurnCommitFailed -- while leaving row B's own eventual Ack (if ever
// reached, which it should not be in this test) untouched.
type ackFailingServiceP588 struct {
	session.Service
	ackErr    error
	ackedOnce bool
}

func (s *ackFailingServiceP588) AckRunQueueEntry(ctx context.Context, id, leasedBy string) (string, error) {
	if !s.ackedOnce {
		s.ackedOnce = true
		return "", s.ackErr
	}
	return s.Service.AckRunQueueEntry(ctx, id, leasedBy)
}

// TestDrainSessionNow_PartialDrain_AckErrorThenBusy_PreservesError pins the
// "Ack error -> busy" contract: row A's turn runs and would otherwise
// commit cleanly, but the Ack write itself fails (ErrTurnCommitFailed --
// the row stays leased, side effects already landed). Then row B is found
// busy. The Ack failure must reach the caller, not be discarded for a
// fabricated success.
func TestDrainSessionNow_PartialDrain_AckErrorThenBusy_PreservesError(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "p588-ack-then-busy")
	failing := &ackFailingServiceP588{Service: svc, ackErr: errors.New("p588: disk on fire during row A's ack")}
	coord := &twoRowCoordinator{firstFn: func() (*any, error) { return nil, nil }}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       failing,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p588-ack-busy",
	})
	enqueueTwoRows(t, "p588-ack-busy", failing, sess.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drained, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.Equal(t, 2, coord.calls, "row A's turn (Ack fails after) and row B (busy) must both have been attempted")
	require.True(t, drained, "row A's turn genuinely ran, even though its Ack write failed")
	require.Error(t, drainErr, "row A's Ack failed -- the row was never actually committed; that must reach the caller, not be replaced by a bare nil")
	require.ErrorIs(t, drainErr, session.ErrTurnCommitFailed, "the SPECIFIC Ack-failure sentinel must be preserved")
}

// VERBATIM revert-check output, captured by temporarily reverting the
// executed-here stopNow branch (the second of the two `if err == nil &&
// drained { err = ErrDrainIncomplete }` sites in run_queue_drain_session.go)
// back to its pre-fix unconditional `return drained, nil`:
//
//	=== RUN   TestDrainSessionNow_PartialDrain_SuccessThenBusy_NeverReportsCleanSuccess
//	=== RUN   TestDrainSessionNow_PartialDrain_RetryableErrorThenBusy_PreservesError
//	=== RUN   TestDrainSessionNow_PartialDrain_TerminalErrorThenBusy_PreservesError
//	=== RUN   TestDrainSessionNow_PartialDrain_AckErrorThenBusy_PreservesError
//	    p588_partial_drain_test.go:154:
//	        	Error Trace:	D:/dev/go/crush/.claude/worktrees/agent-drain-p0/internal/session/p588_partial_drain_test.go:154
//	        	Error:      	An error is expected but got nil.
//	        	Test:       	TestDrainSessionNow_PartialDrain_RetryableErrorThenBusy_PreservesError
//	        	Messages:   	a retryable failure was accumulated for row A; it must reach the caller, not be silently discarded for a bare nil
//	--- FAIL: TestDrainSessionNow_PartialDrain_RetryableErrorThenBusy_PreservesError (0.52s)
//	    p588_partial_drain_test.go:250:
//	        	Error Trace:	D:/dev/go/crush/.claude/worktrees/agent-drain-p0/internal/session/p588_partial_drain_test.go:250
//	        	Error:      	An error is expected but got nil.
//	        	Test:       	TestDrainSessionNow_PartialDrain_AckErrorThenBusy_PreservesError
//	        	Messages:   	row A's Ack failed -- the row was never actually committed; that must reach the caller, not be replaced by a bare nil
//	--- FAIL: TestDrainSessionNow_PartialDrain_AckErrorThenBusy_PreservesError (0.53s)
//	    p588_partial_drain_test.go:191:
//	        	Error Trace:	D:/dev/go/crush/.claude/worktrees/agent-drain-p0/internal/session/p588_partial_drain_test.go:191
//	        	Error:      	An error is expected but got nil.
//	        	Test:       	TestDrainSessionNow_PartialDrain_TerminalErrorThenBusy_PreservesError
//	        	Messages:   	row A terminal-failed; that permanent loss must reach the caller, not be replaced by a bare nil
//	--- FAIL: TestDrainSessionNow_PartialDrain_TerminalErrorThenBusy_PreservesError (0.54s)
//	    p588_partial_drain_test.go:102:
//	        	Error Trace:	D:/dev/go/crush/.claude/worktrees/agent-drain-p0/internal/session/p588_partial_drain_test.go:102
//	        	Error:      	An error is expected but got nil.
//	        	Test:       	TestDrainSessionNow_PartialDrain_SuccessThenBusy_NeverReportsCleanSuccess
//	        	Messages:   	row B was left undrained (busy) after row A succeeded -- this must NOT be reported as a clean success
//	--- FAIL: TestDrainSessionNow_PartialDrain_SuccessThenBusy_NeverReportsCleanSuccess (0.56s)
//	FAIL
//	FAIL	github.com/charmbracelet/crush/internal/session	0.717s
//	FAIL
//
// (All four reverted-scenario failures are on the exact assertion this
// task's contract requires: "an error is expected but got nil" -- the
// pre-fix code discarded the accumulated error and reported a bare nil in
// every one of the four cases. The fix was then restored and all four
// tests re-run to confirm PASS again.)
