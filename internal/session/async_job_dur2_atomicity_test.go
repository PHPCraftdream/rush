// DUR-2 atomicity coverage (docs/plans/2026-09-28-async-phase4-durable-core.md
// sec.3.2/sec.4: "terminal transition, delivery='pending' and wake by cause
// -- one transaction"). Transition (async_job_store.go) already proves the
// WON direction (TestAsyncJobStore_TransitionWonThenLostThenGone: a
// committed transition sets state/delivery/wake together). This file proves
// the fault direction through the REAL Transition: a database-level fault on
// the wake write of the terminal transition (a trigger aborting any UPDATE
// that sets wake) must leave state, delivery, wake and reacted exactly as they
// were, and the row must still be transitionable afterwards. An
// implementation that wrote state/delivery and wake in separate statements
// (each committing on its own) would leave state='completed' behind.
package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTransition_InducedWakeWriteFailureChangesNothing is DUR-2's fault-
// injection proof, driven through store.Transition itself (not a hand-copied
// query): the fault fires on the write that sets wake=1 -- one of the fields
// the terminal transition sets together with state/delivery -- and nothing of
// the transition may be visible afterwards.
//
// REVERT CHECK: Transition temporarily changed to apply the terminal state and
// delivery in one committed statement and the wake bit in a second one -- the
// induced failure then left state='completed'/delivery='pending' behind and
// the `require.Equal(t, "running", after.State, ...)` assertion failed.
// Restored the single CAS statement; re-ran, passed.
func TestTransition_InducedWakeWriteFailureChangesNothing(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	claimed, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)
	require.Equal(t, "running", claimed.Row.State)
	require.Equal(t, "none", claimed.Row.Delivery)
	require.EqualValues(t, 0, claimed.Row.Wake)

	abortingTrigger(t, store, "fail_terminal_wake", "UPDATE OF wake ON async_jobs WHEN NEW.wake = 1", "induced wake write failure")
	_, err = store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "completed",
		ResultSummary: "ok", ResultIsError: true, Wake: true,
	})
	require.Error(t, err, "the induced failure must surface, not be swallowed")
	require.Contains(t, err.Error(), "induced wake write failure")

	after, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", after.State, "a failed transition must not change state")
	require.Equal(t, "none", after.Delivery, "a failed transition must not change delivery")
	require.EqualValues(t, 0, after.Wake, "a failed transition must not change wake")
	require.EqualValues(t, 0, after.Reacted, "a failed transition must not change reacted")
	require.False(t, after.ResultSummary.Valid, "a failed transition must not leave a result behind")

	// The fault is gone: the same transition now wins and commits the fields
	// together.
	_, err = store.sqlDB.ExecContext(context.Background(), "DROP TRIGGER fail_terminal_wake")
	require.NoError(t, err)
	result, err := store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "completed",
		ResultSummary: "ok", ResultIsError: true, Wake: true,
	})
	require.NoError(t, err)
	require.Equal(t, TransitionWon, result.Outcome)
	require.Equal(t, "completed", result.Row.State)
	require.Equal(t, "pending", result.Row.Delivery)
	require.EqualValues(t, 1, result.Row.Wake)
}
