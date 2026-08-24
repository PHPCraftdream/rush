package session_test

// Regression coverage for task #610 (P0) -- the seventh form of
// DrainSessionNow's recurring "reported complete over durable, unresolved
// work" defect, and the first reachable under a live, non-cancelled ctx.
//
// Every prior fix in this same file's history (#575, #578, #588, #592,
// #607, #613) closed a way DrainSessionNow's terminal ledger.verdict(false)
// call could be reached with stale or missing information about a row THIS
// call itself touched, or about ITS OWN ctx dying. None of them covered a
// row this call never touched at all: GetOldestPendingRunQueueEntryForSession
// (the query LeaseRunQueueEntry uses) only ever looks at status = 'pending',
// so a row leased by a genuinely different owner -- another process, or
// (for this package's own test purposes) a different leasedBy value calling
// the same real service -- is invisible to it. Before this task's fix, the
// bottom-of-loop "nothing pending" branch fell straight through to
// ledger.verdict(false), which resolves to (DrainComplete, nil) whenever
// nothing else in the ledger recorded a failure -- even though that foreign
// row was durable, outstanding, and its outcome completely unknown to this
// call.
//
// These tests drive the real production path: a real session.Service
// backed by a real SQLite DB (setupTestSession), a real
// RunQueuePump.DrainSessionNow call, and a foreign lease taken via the SAME
// real service's own LeaseRunQueueEntry under a DIFFERENT leasedBy --
// exactly what a different process's pump instance would look like from
// this call's point of view.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestDrainSessionNow_ForeignLeasedRow_NeverReportsComplete pins the exact
// reviewer-reproduced sequence: row A executes and commits cleanly through
// THIS DrainSessionNow call, then row B (a second entry for the SAME
// session) is leased by a genuinely different owner and never resolved --
// simulated here via the same real service's LeaseRunQueueEntry under a
// different leasedBy, with a lease TTL long enough that it is still
// unexpired by the time DrainSessionNow's own loop reaches the bottom.
//
// Before the fix, this returned (DrainComplete, nil): row A's clean
// recordSuccess left the ledger's failed map empty, and the pending-only
// lease attempt for row B silently found nothing (B is 'leased', not
// 'pending'), so DrainSessionNow's bottom branch had no way to learn B
// existed at all.
//
// Revert-check: comment out the HasOutstandingRunQueueEntriesForSession
// call (and its ledger.recordUnattributed on hasOutstanding) in
// DrainSessionNow's bottom branch, in run_queue_drain_session.go, and this
// test fails with result=DrainComplete, err=nil.
func TestDrainSessionNow_ForeignLeasedRow_NeverReportsComplete(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "drain-p610-foreign-leased-live")
	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p610-foreign-leased-live",
	})

	ctx := context.Background()

	// Row B: enqueue FIRST and lease it immediately (it is the only pending
	// row at this point, so LeaseRunQueueEntry's oldest-pending pick is
	// unambiguous), standing in for a genuinely different process's pump
	// instance that has claimed it but not yet Ack'd/Nack'd/terminal-failed
	// it. A long TTL keeps it unexpired for the duration of this test, so
	// this is unambiguously "outstanding", not "expired and up for
	// recovery".
	callDataB, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "row B"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "p610-live-row-B", sess.ID, callDataB))
	leasedB, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "a-different-process-entirely", 5*time.Minute)
	require.NoError(t, err)
	require.NotNil(t, leasedB, "row B must have been successfully leased by the foreign owner for this test to prove anything")
	require.Equal(t, "p610-live-row-B", leasedB.ID)

	// Row A: enqueue AFTER B is already leased, so it is the only 'pending'
	// row left for DrainSessionNow's own lease attempt to find and execute.
	callDataA, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "row A"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "p610-live-row-A", sess.ID, callDataA))

	drainCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(drainCtx, sess.ID)

	require.Error(t, drainErr, "row B is durable, outstanding, and unresolved -- DrainSessionNow must never report success while it exists, regardless of what row A did")
	require.ErrorIs(t, drainErr, session.ErrOutstandingRunQueueEntry)
	require.NotEqual(t, session.DrainComplete, result, "the coordinator's exact false-success scenario: a DIFFERENT row's clean commit must not mask a foreign, outstanding row for the same session")
	require.Equal(t, int64(1), coord.calls.Load(), "row A must have executed exactly once; row B must NOT have been touched by this call -- it was never this call's lease to execute")

	// Row B is untouched -- still leased by the foreign owner, not acked,
	// not nacked, not terminal-failed. Confirms this call correctly left it
	// alone rather than somehow interfering with a lease it does not own.
	current, err := svc.GetRunQueueEntry(ctx, "p610-live-row-B")
	require.NoError(t, err)
	require.NotNil(t, current)
	require.Equal(t, session.RunQueueStatusLeased, current.Status)
}

// TestDrainSessionNow_ForeignLeasedRow_ExpiredButNotYetCleaned pins the
// deliberate contract decision the task names explicitly: an outstanding
// row whose lease has ALREADY EXPIRED, but which CleanupExpiredLeases has
// not yet swept back to pending, still counts as outstanding. Exit-code
// correctness cannot depend on timing a maintenance sweep this call does
// not control and is not obligated to wait for -- an eighth review found a
// version of this fix that treated expiry as equivalent to "gone" still
// let `rush run` exit 0 over a row whose fate remained genuinely unknown
// at the moment this call returned.
func TestDrainSessionNow_ForeignLeasedRow_ExpiredButNotYetCleaned(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "drain-p610-foreign-leased-expired")
	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p610-foreign-leased-expired",
	})

	ctx := context.Background()

	// Row B: enqueue and lease it FIRST (see the sibling test's comment for
	// why this ordering is required for LeaseRunQueueEntry's oldest-pending
	// pick to unambiguously target B). A near-zero TTL: by the time
	// DrainSessionNow's loop reaches the bottom (after executing row A for
	// real against a real DB), this lease is guaranteed to already be
	// expired in wall-clock terms -- but nothing in this test ever calls
	// CleanupExpiredLeases, so the row is still sitting in status='leased'
	// in the DB, exactly as an un-swept expired lease would be in
	// production between pump ticks.
	callDataB, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "row B"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "p610-expired-row-B", sess.ID, callDataB))
	leasedB, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "a-different-process-entirely", 1*time.Nanosecond)
	require.NoError(t, err)
	require.NotNil(t, leasedB)
	require.Equal(t, "p610-expired-row-B", leasedB.ID)

	callDataA, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "row A"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "p610-expired-row-A", sess.ID, callDataA))

	drainCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(drainCtx, sess.ID)

	require.Error(t, drainErr, "an EXPIRED-but-not-yet-recovered lease is still outstanding, not equivalent to gone -- CleanupExpiredLeases has not run, so this call cannot confirm any outcome for it")
	require.ErrorIs(t, drainErr, session.ErrOutstandingRunQueueEntry)
	require.NotEqual(t, session.DrainComplete, result, "exit code correctness must not depend on whether a maintenance sweep happened to run before this call returned")

	current, err := svc.GetRunQueueEntry(ctx, "p610-expired-row-B")
	require.NoError(t, err)
	require.NotNil(t, current, "the row must still exist -- this test never calls CleanupExpiredLeases")
	require.Equal(t, session.RunQueueStatusLeased, current.Status, "still leased in the DB -- only CleanupExpiredLeases (not DrainSessionNow) recovers an expired lease back to pending")
}

// outstandingCheckFailingService wraps a real session.Service and fails
// HasOutstandingRunQueueEntriesForSession specifically -- everything else
// (leasing, executing, acking) goes to the real implementation, so a row
// genuinely executes and commits before the outstanding check is ever
// reached, exactly like the coordinator's SQLITE_BUSY scenario: the
// confirmation query itself fails, not any row this call touched.
type outstandingCheckFailingService struct {
	session.Service
	err   error
	calls int
}

func (s *outstandingCheckFailingService) HasOutstandingRunQueueEntriesForSession(ctx context.Context, sessionID string) (bool, error) {
	s.calls++
	return false, s.err
}

// TestDrainSessionNow_OutstandingCheckFailure_NeverReportsComplete pins the
// coordinator's follow-up finding on task #610: a FAILED confirmation
// attempt (HasOutstandingRunQueueEntriesForSession itself returning an
// error, e.g. SQLITE_BUSY under exactly the contention a genuinely
// different live owner holding a row would produce) must not be read as
// "confirmed empty". Before this fix, the failure was only logged via
// slog.Warn and execution fell through to ledger.verdict(false) with a
// ledger holding one clean success and no failures -- (DrainComplete, nil)
// -- the eighth form of the same "reported complete over unresolved work"
// defect task #610 already fixed once for the query's RESULT path.
//
// Revert-check: in run_queue_drain_session.go's outstanding-check switch,
// remove the `ledger.recordUnattributed(...)` call from the
// `outstandingErr != nil` case (leaving only the slog.Warn, i.e. the
// pre-follow-up-fix behavior) and this test fails with result=DrainComplete,
// err=nil.
func TestDrainSessionNow_OutstandingCheckFailure_NeverReportsComplete(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "drain-p610-outstanding-check-fails")
	failing := &outstandingCheckFailingService{
		Service: svc,
		err:     errors.New("SQLITE_BUSY: database is locked"),
	}
	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       failing,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p610-outstanding-check-fails",
	})

	ctx := context.Background()
	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "only row"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "p610-check-fail-row-A", sess.ID, callData))

	drainCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(drainCtx, sess.ID)

	require.Error(t, drainErr, "the confirmation query itself failed -- 'I could not confirm the queue is empty' must never be reported as 'the queue is empty'")
	require.ErrorIs(t, drainErr, session.ErrOutstandingCheckUnconfirmed, "must be the distinct 'unconfirmed' sentinel, not ErrOutstandingRunQueueEntry -- the ledger never learned whether a row is actually outstanding, only that it could not ask")
	require.NotErrorIs(t, drainErr, session.ErrOutstandingRunQueueEntry, "must NOT claim a confirmed outstanding row exists -- that would be a different, stronger, false claim")
	require.ErrorContains(t, drainErr, "SQLITE_BUSY", "the underlying DB error must survive wrapping, or the log is the only place it exists")
	require.ErrorContains(t, drainErr, sess.ID, "the sentinel must carry sessionID so an operator can tell which session's confirmation failed")
	require.NotEqual(t, session.DrainComplete, result, "row A's own clean success must not mask the fact that this call could not confirm the queue is otherwise empty")
	require.Equal(t, int64(1), coord.calls.Load(), "row A must have executed exactly once")
	require.GreaterOrEqual(t, failing.calls, 1, "the outstanding check must actually have been attempted for this test to prove anything")
}

// TestDrainSessionNow_TrulyEmptyQueue_StillReportsComplete is the
// must-not-regress control: when the session's run queue really is empty in
// EVERY status after this call's own row(s) resolve, DrainComplete must
// still be returned. Guards against an overzealous fix turning every
// successful drain into a false failure, which the task explicitly warns
// would be worse than the defect being fixed.
func TestDrainSessionNow_TrulyEmptyQueue_StillReportsComplete(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "drain-p610-truly-empty")
	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p610-truly-empty",
	})

	ctx := context.Background()
	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "only row"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "p610-empty-row-A", sess.ID, callData))

	drainCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(drainCtx, sess.ID)

	require.NoError(t, drainErr, "the queue is genuinely empty after this call's own row committed -- reporting a failure here would be worse than the defect this task fixes")
	require.Equal(t, session.DrainComplete, result)
	require.Equal(t, int64(1), coord.calls.Load())

	// Confirm via the same production query the fix uses that the queue is,
	// in fact, empty in every status -- not merely that no error surfaced.
	has, err := svc.HasOutstandingRunQueueEntriesForSession(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, has, "the queue must be genuinely empty across every status for this control test to mean anything")
}
