package session_test

// Regression coverage for task #624 (F-5 and F-7 of the 2026-08-20 ninth
// review of 3b2664da), on DrainSessionNow's visibility and error-priority
// contracts.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// F-5: the anyExecuted gate on the terminal HasOutstandingRunQueueEntriesForSession
// check hid a leased row this call has no live local explanation for at all.
// The concrete shape: a row left in status 'leased' by an owner that is now
// gone -- e.g. a prior DrainSessionNow whose executeEntrySync panicked before
// any Ack/Nack could land. A later, COLD DrainSessionNow call (fresh pump,
// nothing executing in this pump instance for the session, no admission ever
// observed) leased nothing (the row is 'leased', not 'pending'), executed
// nothing (anyExecuted == false), and therefore skipped the outstanding
// check entirely -- returning (DrainNoWork, nil) over a durable row whose
// fate it never even looked at.
//
// The fix keeps the raced-admission case's deliberate DrainNoWork (a call
// that WAITED on a same-pump admission holder may not report that holder's
// in-flight row as its own failure -- pinned by
// TestProcessEntry_RacedLeaseNil_DoesNotFalselyDrainAWaiter) while making
// the cold case visible: the outstanding query now also runs when this call
// never observed any admission for the session.

// TestDrainSessionNow_ColdCall_OrphanedLeasedRow_IsVisible pins F-5's fix:
// a row leased by an owner this call never saw any sign of (simulated via
// the real service's own LeaseRunQueueEntry under a foreign leasedBy --
// exactly the DB shape a panicked-away owner leaves behind) must make a
// COLD DrainSessionNow report DrainFailed with ErrOutstandingRunQueueEntry,
// not (DrainNoWork, nil).
//
// Revert-check: restore the bare anyExecuted gate in run_queue_drain_session.go
// (i.e. `if ledger.anyExecuted && !ledger.hasFailures()`) and this test
// fails with result=DrainNoWork, err=nil.
func TestDrainSessionNow_ColdCall_OrphanedLeasedRow_IsVisible(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p624-cold-orphaned-lease")
	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p624-cold-orphaned-lease",
	})

	ctx := context.Background()

	// The orphaned row: enqueued, then leased by a foreign owner that will
	// never resolve it -- the exact durable state a panicked pump leaves
	// behind (nothing in this test ever Acks/Nacks/terminal-fails it, and
	// the pump is never even Start()ed, so nothing in this pump instance
	// has ever held an admission for this session either). A long TTL keeps
	// it unambiguously "outstanding", not "expired and up for recovery".
	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "orphaned"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "p624-orphaned-row", sess.ID, callData))
	leased, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "a-pump-that-panicked-away", 5*time.Minute)
	require.NoError(t, err)
	require.NotNil(t, leased, "the orphaned row must be genuinely leased for this test to prove anything")
	require.Equal(t, "p624-orphaned-row", leased.ID)

	drainCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(drainCtx, sess.ID)

	require.Equal(t, session.DrainFailed, result, "a cold call that finds this session's queue still holding an unresolved leased row must not report 'nothing for this call to do' over it")
	require.ErrorIs(t, drainErr, session.ErrOutstandingRunQueueEntry)
	require.Zero(t, coord.calls.Load(), "the orphaned row was never this call's lease to execute")

	// The row is untouched -- still leased by its (gone) foreign owner.
	current, err := svc.GetRunQueueEntry(ctx, "p624-orphaned-row")
	require.NoError(t, err)
	require.NotNil(t, current)
	require.Equal(t, session.RunQueueStatusLeased, current.Status)
}

// TestDrainSessionNow_ColdCall_ForeignLiveOwner_ReportsOutstanding pins the
// branch the #624 follow-up review reasoned about specifically: a LIVE
// cross-process owner holding an unexpired lease on a row of this session,
// with no admission this call could ever observe (LeaseRunQueueEntry never
// consults the OS session lock -- that sits one layer up, in
// Coordinator.Run -- so a foreign pump's live lease is indistinguishable
// from an orphaned one by the only query available). The decision recorded
// at the gate in run_queue_drain_session.go: this deliberately reports
// DrainFailed/ErrOutstandingRunQueueEntry, mirroring task #610's
// already-accepted reporting of the exact same shape for calls that DID
// execute something (TestDrainSessionNow_ForeignLeasedRow_NeverReportsComplete
// also uses a live, unexpired foreign lease).
//
// This test differs from ..._OrphanedLeasedRow_IsVisible above in INTENT,
// not mechanism: that test frames the row as abandoned by a dead owner;
// this one pins that a row owned by a LIVE foreign process -- the case the
// app_run.go DrainNoWork comment used to claim keeps the original
// cancellation standing -- is equally visible. Consumer-side, the
// observable difference is only WHICH non-nil error `rush run` reports;
// see drainOutcomeError's own comment in internal/app/app_run.go.
//
// Revert-check (per-test, not per-edit-site): removing the `||
// !sawOtherAdmission` arm from the gate in run_queue_drain_session.go --
// and changing NOTHING else -- makes THIS test fail with
// result=DrainNoWork, err=nil. The complementary direction also holds:
// neutralising `sawOtherAdmission = true` at its set site does NOT affect
// this test (a cold call never sets the flag) while it DOES fail
// TestProcessEntry_RacedLeaseNil_DoesNotFalselyDrainAWaiter -- proving the
// two tests observe DIFFERENT branches of the gate rather than both riding
// one mechanism.
func TestDrainSessionNow_ColdCall_ForeignLiveOwner_ReportsOutstanding(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p624-cold-foreign-live-owner")
	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p624-foreign-live-owner",
	})

	ctx := context.Background()

	// The live foreign owner's row: leased under a foreign leasedBy with a
	// TTL that is still comfortably unexpired at assertion time -- this is
	// a LIVE owner mid-flight, not an abandoned lease.
	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "foreign live"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "p624-foreign-live-row", sess.ID, callData))
	leased, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "another-process-pump-live", 5*time.Minute)
	require.NoError(t, err)
	require.NotNil(t, leased)
	require.Equal(t, "p624-foreign-live-row", leased.ID)

	drainCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(drainCtx, sess.ID)

	require.Equal(t, session.DrainFailed, result, "a live cross-process owner's outstanding row is undrained durable work whose outcome is unknown to this call -- reporting it is the honest answer, mirroring #610's accepted reporting for the anyExecuted arm")
	require.ErrorIs(t, drainErr, session.ErrOutstandingRunQueueEntry)
	require.Zero(t, coord.calls.Load(), "the foreign-owned row was never this call's lease to execute")

	current, err := svc.GetRunQueueEntry(ctx, "p624-foreign-live-row")
	require.NoError(t, err)
	require.NotNil(t, current)
	require.Equal(t, session.RunQueueStatusLeased, current.Status, "the live foreign owner's row must be untouched")
}

// TestDrainSessionNow_ColdCall_EmptyQueue_StillNoWork is the must-not-regress
// control for the F-5 fix: a cold call against a genuinely empty queue still
// reports (DrainNoWork, nil). Guards against an overzealous fix turning
// every empty-queue drain into a false failure.
func TestDrainSessionNow_ColdCall_EmptyQueue_StillNoWork(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p624-cold-empty-queue")
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    &countingCoordinatorForDrain{},
		PumpInstanceID: "test-pump-p624-cold-empty",
	})

	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(drainCtx, sess.ID)

	require.Equal(t, session.DrainNoWork, result)
	require.NoError(t, drainErr)

	has, err := svc.HasOutstandingRunQueueEntriesForSession(context.Background(), sess.ID)
	require.NoError(t, err)
	require.False(t, has, "control only: the queue must genuinely be empty for this test to mean anything")
}

// F-7: verdict() reports ONE representative error, and before #624 it picked
// the FRESHEST surviving failure unconditionally. Every ctx-death exit folds
// ctx.Err() into the ledger as a synthetic unattributed entry
// (verdictOnCtxDone at the top-of-loop and admission-wait sites, the explicit
// recordUnattributed at the execSem-wait site), so a call whose own ctx dies
// AFTER a row-attributed failure was already recorded reported the generic
// context error -- masking the specific, row-attributed cause (terminal
// loss, Ack failure, lease loss) a caller and this package's tests match
// with errors.Is. The fix is a priority rule INSIDE the ledger: a
// row-attributed failure always outranks an unattributed one; within a
// tier, freshest still wins.

// cancellingTerminalCoordinator runs once: it cancels the DRAIN caller's ctx
// (handed in by the test) and returns a terminal AlreadyAttempted error --
// deterministically staging "row-attributed failure recorded, then this
// call's own ctx dies before the next loop iteration" without any sleeps.
// The cancellation lands inside Coordinator.Run, i.e. before
// executeEntrySync's TerminalFail write (which uses its own
// context.Background-derived budget and therefore still lands), and thus
// before the top-of-loop ctx check sees it.
type cancellingTerminalCoordinator struct {
	calls  atomic.Int64
	cancel context.CancelFunc
	err    error
}

func (c *cancellingTerminalCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	c.cancel()
	return nil, c.err
}

// TestDrainSessionNow_CtxDeathAfterRowFailure_ReportsRowFailureNotCtxErr
// pins F-7's priority rule on the top-of-loop ctx exit: row A terminal-fails
// (row-attributed failure under A's own ID), this call's own ctx is already
// dead by the time the loop iterates (cancelled inside Coordinator.Run, on
// the same goroutine -- no timing window at all), and verdictOnCtxDone folds
// ctx.Err() in as a fresher unattributed entry. The reported cause must be
// row A's terminal error, not context.Canceled.
//
// Revert-check: restore mostRecentFailure's pre-#624 freshest-wins-unconditionally
// walk in run_queue_drain_session.go and this test fails with drainErr =
// context.Canceled (the ErrorIs(termErr) assertion below).
func TestDrainSessionNow_CtxDeathAfterRowFailure_ReportsRowFailureNotCtxErr(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p624-ctx-death-after-row-failure")
	termErr := &agent.ErrCallAlreadyAttempted{Err: errors.New("p624: row A already attempted")}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	coord := &cancellingTerminalCoordinator{cancel: cancel, err: termErr}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p624-priority-rule",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "row A"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "p624-priority-row-A", sess.ID, callData))

	result, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.Equal(t, int64(1), coord.calls.Load(), "row A must have been attempted exactly once")
	require.Equal(t, session.DrainFailed, result, "both a row failure and a ctx-death are on record -- DrainFailed either way; this assertion only rules out a false complete/partial")
	require.ErrorIs(t, drainErr, termErr, "the row-attributed terminal cause must WIN over the fresher, general ctx.Err() entry -- the whole point of the #624/F-7 priority rule")
	require.NotErrorIs(t, drainErr, context.Canceled, "a generic context error must not mask the specific durable-row cause a caller needs to act on")

	// The terminal write must genuinely have landed (Background-rooted
	// budget, unaffected by the drain ctx's cancellation) -- otherwise the
	// row-attributed failure entry would describe a row still in place and
	// this test would not be exercising the terminal shape it names.
	current, err := svc.GetRunQueueEntry(context.Background(), "p624-priority-row-A")
	require.NoError(t, err)
	require.Nil(t, current, "row A must have been terminal-failed (deleted), not left leased or pending")
}

// TestDrainSessionNow_OnlyUnattributed_StillReportsIt pins the fallback half
// of the priority rule: when NO row-attributed failure survives, the freshest
// unattributed entry is still the representative -- exactly the pre-existing
// #607 behavior (one clean success, then ctx dies) that the tier rule must
// not disturb.
func TestDrainSessionNow_OnlyUnattributed_StillReportsIt(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p624-only-unattributed")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Two rows: row A runs and commits cleanly under the drain's own ctx;
	// its Run call then leaves the ctx cancelled, so the loop's next
	// top-of-loop check hits verdictOnCtxDone with a ledger holding one
	// clean success and no failures -- the exact #607 shape.
	coord := &cancellingTerminalCoordinator{cancel: cancel, err: nil}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p624-only-unattributed",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "only row"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "p624-only-row-A", sess.ID, callData))

	result, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.Equal(t, session.DrainFailed, result, "a ctx death after a clean commit is not a completed drain (task #607)")
	require.ErrorIs(t, drainErr, context.Canceled, "with no row-attributed failure on record, the unattributed ctx error must still be the representative -- the priority rule only re-ranks, it must not drop the tier-2 entry")
}
