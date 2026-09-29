// B8/C5b,c (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): the
// CLI loop must not spin at 100% CPU when a notice's pull keeps failing
// (the row stays 'pending' forever).
//
// An earlier version of this fix made waitForNextCLITurn's own "is a turn
// owed" check use the VISIBLE-only debt predicate (delivery='done'),
// matching decideDrainTurn's own check. That broke the COMMON case: a
// freshly-arrived notice is normally still 'pending' the first time
// waitForNextCLITurn sees it (nothing has pulled it into history yet), so
// the VISIBLE-only check always reported "no debt" for it, fell through to
// ScopeOpen (which is pending-inclusive and so reported "open" anyway), and
// blocked on WaitForHint waiting for a hint that had ALREADY fired before
// this call started watching for it -- several seconds of pure waste (up to
// reactionDebtSourceHintFallback) on every single ordinary async
// completion, breaking TestRunNonInteractiveWaitsForAsyncCommand... and
// friends in internal/app's full suite.
//
// The corrected fix keeps waitForNextCLITurn on the pending-inclusive
// predicate (correct for the common case) and instead paces the loop AFTER
// a no-turn Drain iteration specifically (waitAfterNoTurnDrain): only a
// Drain that actually ran and found nothing to react to waits for a hint
// before the next check, which never fires for a fresh notice's first
// (successful) attempt.
package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// fakeStuckPendingReactionSource simulates a session whose debt row is
// permanently stuck at delivery='pending' (its pull keeps failing): the
// pending-inclusive predicate always reports debt present, and WaitForHint
// counts its own calls (production's real WaitForHint blocks up to its own
// bounded fallback; this fake returns immediately so tests stay fast).
type fakeStuckPendingReactionSource struct {
	plainDebtCalls   int32
	waitForHintCalls int32
}

func (f *fakeStuckPendingReactionSource) ClaimExternalDriver(context.Context, string) error {
	return nil
}
func (f *fakeStuckPendingReactionSource) ReleaseExternalDriver(context.Context, string) {}
func (f *fakeStuckPendingReactionSource) RunMaintenanceSweep(context.Context)           {}

func (f *fakeStuckPendingReactionSource) CLIScope(context.Context, string) (agent.CLIScopeState, error) {
	atomic.AddInt32(&f.plainDebtCalls, 1)
	return agent.CLIScopeState{TurnOwed: true, WorkOpen: true}, nil // pending-inclusive: the stuck row still counts
}

func (f *fakeStuckPendingReactionSource) ScopeOpen(context.Context, string) (bool, error) {
	return true, nil
}

// alwaysErrorsReactionSource simulates a persistently unreadable DB: every
// CLIScope call fails.
type alwaysErrorsReactionSource struct {
	fakeStuckPendingReactionSource
	calls int32
}

func (f *alwaysErrorsReactionSource) CLIScope(context.Context, string) (agent.CLIScopeState, error) {
	atomic.AddInt32(&f.calls, 1)
	return agent.CLIScopeState{}, errors.New("database is locked")
}

func (f *fakeStuckPendingReactionSource) WaitForHint(context.Context, string) {
	atomic.AddInt32(&f.waitForHintCalls, 1)
}

func (f *fakeStuckPendingReactionSource) CaptureDrainSnapshot(context.Context, string) session.DebtSnapshot {
	return session.DebtSnapshot{}
}

func (f *fakeStuckPendingReactionSource) RecordDrainTurnOutcome(context.Context, string, session.DebtSnapshot, error, bool) {
}

// TestWaitForNextCLITurn_PendingDebt_ReturnsImmediately is the regression
// guard for the common case an earlier version of this fix broke: a
// pending (not yet visible) debt row must be reported as a turn owed right
// away, without ever blocking on WaitForHint.
//
// Revert-check performed (against the REJECTED design, not the current
// code): using the VISIBLE-only predicate here instead of the pending-
// inclusive one made this exact assertion fail (waitForHintCalls > 0,
// hasNext observed only after a wait) -- see the file-level doc for the
// full story. The current code (pending-inclusive) passes without ever
// touching WaitForHint.
func TestWaitForNextCLITurn_PendingDebt_ReturnsImmediately(t *testing.T) {
	f := &fakeStuckPendingReactionSource{}
	app := &App{}

	hasNext, err := app.waitForNextCLITurn(context.Background(), f, "sess-1")

	require.NoError(t, err)
	require.True(t, hasNext, "a pending (not yet visible) debt row must be reported as a turn owed immediately")
	require.Zero(t, atomic.LoadInt32(&f.waitForHintCalls),
		"a fresh pending notice must never block on WaitForHint -- the next turn's own pull is what makes it visible")
}

// TestWaitAfterNoTurnDrain_OnlyWaitsWhenDrainWasNoTurn pins the actual B8/
// C5b,c fix: the loop paces itself with WaitForHint ONLY after a no-turn
// Drain (drainNoTurn=true), never after a real turn.
//
// Revert-check performed: changed waitAfterNoTurnDrain (app_run_async.go)
// to `source.WaitForHint(ctx, sessionID)` unconditionally -- this test's
// `require.Zero(t, f.waitForHintCalls)` (the drainNoTurn=false case) FAILED.
// Restored the `if drainNoTurn` guard; re-ran, passed.
func TestWaitAfterNoTurnDrain_OnlyWaitsWhenDrainWasNoTurn(t *testing.T) {
	f := &fakeStuckPendingReactionSource{}
	waitAfterNoTurnDrain(context.Background(), f, "sess-1", false)
	require.Zero(t, atomic.LoadInt32(&f.waitForHintCalls), "a real turn must never trigger the no-turn-Drain pacing wait")

	waitAfterNoTurnDrain(context.Background(), f, "sess-1", true)
	require.EqualValues(t, 1, atomic.LoadInt32(&f.waitForHintCalls), "a no-turn Drain must pace itself via WaitForHint before the next check")
}

// TestPermanentPullFailure_EveryNoTurnIterationPaces is W-DRAIN item 2's
// (C5b, docs/reviews/2026-09-29-async-phase4-round1.md) verification of
// design doc sec.6's named test: "a Drain ... with a permanent pull error
// does not loop" -- a row whose pull keeps failing stays 'pending' forever
// (fakeStuckPendingReactionSource.CLIScope always reports a turn owed), so
// waitForNextCLITurn always reports a turn owed and the loop always attempts
// (and, per this simulation, always fails to visibly react to) it, exactly
// the drainNoTurn=true shape app_run_async.go's loop produces for a stuck
// pending row. Driving the loop's own two primitives together (as the real
// loop body does, once per iteration) across many simulated iterations
// proves EVERY one pays a WaitForHint pacing call -- the mechanism that
// keeps this bounded to real wall-clock time instead of a tight CPU spin --
// never a free ride through waitForNextCLITurn's fast pending-debt path
// that bypasses it.
//
// Revert-check performed (against the REJECTED design this pins, not
// current code): using the VISIBLE-only debt predicate in
// waitForNextCLITurn (an earlier, rejected version of the B8/C5b,c fix)
// broke this exact loop shape by routing the stuck row through ScopeOpen's
// own WaitForHint instead of the no-turn-Drain one -- still paced, but by
// the WRONG mechanism (see waitForNextCLITurn's own doc for the full
// story); this test's iteration-count assertion below distinguishes the
// two by requiring the no-turn-Drain wait specifically to fire every time,
// which only the CURRENT (accepted) design routes through.
func TestPermanentPullFailure_EveryNoTurnIterationPaces(t *testing.T) {
	f := &fakeStuckPendingReactionSource{}
	application := &App{}
	const iterations = 25
	for i := 0; i < iterations; i++ {
		// Mirrors app_run_async.go's loop body shape for a Drain iteration
		// that ran but found nothing VISIBLE to react to (ErrRunQueued) --
		// a permanently 'pending' row reproduces this every single pass.
		const drainNoTurn = true
		waitAfterNoTurnDrain(context.Background(), f, "sess-1", drainNoTurn)
		hasNext, err := application.waitForNextCLITurn(context.Background(), f, "sess-1")
		require.NoError(t, err)
		require.True(t, hasNext, "iteration %d: a permanently pending row must still be reported as owed", i)
	}
	require.EqualValues(t, iterations, atomic.LoadInt32(&f.waitForHintCalls),
		"every no-turn iteration must pace itself through WaitForHint -- a permanent pull failure must never bypass it")
}

// TestWaitForNextCLITurn_PersistentDBError_BoundedWithVisibleError is C17's
// fix (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN item 4): a
// persistently unreadable DB must not retry forever with only a log WARN --
// it must give up after a bound and return a visible error the caller can
// surface (cmd/run.go's non-zero exit), not hang indefinitely.
//
// Revert-check performed: removed the `giveUpOnPersistentDBError` bound
// check from waitForNextCLITurn (app_run_async.go), leaving the bare
// `sleepOrCtxDone`-then-`continue` retry -- this test timed out (never
// returned) instead of returning an error within the shrunk bound. Restored
// the bound; re-ran, returned promptly with a non-nil error.
func TestWaitForNextCLITurn_PersistentDBError_BoundedWithVisibleError(t *testing.T) {
	origLimit, origPause := cliDBErrorRetryOverallLimit, cliDBRetryPause
	cliDBErrorRetryOverallLimit = 50 * time.Millisecond
	cliDBRetryPause = 5 * time.Millisecond
	t.Cleanup(func() { cliDBErrorRetryOverallLimit, cliDBRetryPause = origLimit, origPause })

	f := &alwaysErrorsReactionSource{}
	application := &App{}

	hasNext, err := application.waitForNextCLITurn(context.Background(), f, "sess-1")
	require.Error(t, err, "a persistently unreadable DB must eventually surface as an error, not hang forever")
	require.False(t, hasNext)
	require.Greater(t, atomic.LoadInt32(&f.calls), int32(1), "must have actually retried, not failed on the first attempt")
}

// scriptedScopeSource answers CLIScope from a script (the last entry repeats),
// counting its own WaitForHint calls through the embedded fake.
type scriptedScopeSource struct {
	fakeStuckPendingReactionSource
	script []agent.CLIScopeState
	calls  int32
}

func (f *scriptedScopeSource) CLIScope(context.Context, string) (agent.CLIScopeState, error) {
	i := int(atomic.AddInt32(&f.calls, 1)) - 1
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	return f.script[i], nil
}

// TestWaitForNextCLITurn_DeferredDebtIsNeitherWaitedOnNorTurnOwed: debt the
// policy will not allow a turn for must not keep the loop open. With nothing
// running the loop exits at once without ever blocking on WaitForHint; while
// work is still running it waits (once per re-check), then exits when the work
// is gone, and an allowed turn wins immediately.
//
// Revert-check performed: made waitForNextCLITurn treat DeferredDebt like an
// owed turn (`if state.TurnOwed || state.DeferredDebt`) -- the first subtest
// FAILED (hasNext was true); treating it as open work instead FAILED it too
// (WaitForHint was called and the wait never ended).
func TestWaitForNextCLITurn_DeferredDebtIsNeitherWaitedOnNorTurnOwed(t *testing.T) {
	application := &App{}

	t.Run("deferred debt and nothing running exits at once", func(t *testing.T) {
		f := &scriptedScopeSource{script: []agent.CLIScopeState{{DeferredDebt: true}}}
		hasNext, err := application.waitForNextCLITurn(context.Background(), f, "sess-1")
		require.NoError(t, err)
		require.False(t, hasNext, "refused debt must not be reported as a turn owed")
		require.Zero(t, atomic.LoadInt32(&f.waitForHintCalls), "refused debt must not be waited on")
		require.EqualValues(t, 1, atomic.LoadInt32(&f.calls))
	})

	t.Run("running work is waited on, then the loop exits with the debt deferred", func(t *testing.T) {
		f := &scriptedScopeSource{script: []agent.CLIScopeState{
			{WorkOpen: true, DeferredDebt: true},
			{WorkOpen: true, DeferredDebt: true},
			{DeferredDebt: true},
		}}
		hasNext, err := application.waitForNextCLITurn(context.Background(), f, "sess-1")
		require.NoError(t, err)
		require.False(t, hasNext)
		require.EqualValues(t, 2, atomic.LoadInt32(&f.waitForHintCalls), "one wait per re-check while work runs")
	})

	t.Run("an allowed turn beside running work wins", func(t *testing.T) {
		f := &scriptedScopeSource{script: []agent.CLIScopeState{{WorkOpen: true, TurnOwed: true}}}
		hasNext, err := application.waitForNextCLITurn(context.Background(), f, "sess-1")
		require.NoError(t, err)
		require.True(t, hasNext)
		require.Zero(t, atomic.LoadInt32(&f.waitForHintCalls))
	})
}
