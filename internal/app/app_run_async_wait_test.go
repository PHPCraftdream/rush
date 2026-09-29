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
	"sync/atomic"
	"testing"

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

func (f *fakeStuckPendingReactionSource) ClaimExternalDriver(string)          {}
func (f *fakeStuckPendingReactionSource) ReleaseExternalDriver(string)        {}
func (f *fakeStuckPendingReactionSource) RunMaintenanceSweep(context.Context) {}

func (f *fakeStuckPendingReactionSource) ReactionDebtExists(context.Context, string) (bool, error) {
	atomic.AddInt32(&f.plainDebtCalls, 1)
	return true, nil // pending-inclusive: the stuck row still counts
}

func (f *fakeStuckPendingReactionSource) ScopeOpen(context.Context, string) (bool, error) {
	return true, nil
}

func (f *fakeStuckPendingReactionSource) WaitForHint(context.Context, string) {
	atomic.AddInt32(&f.waitForHintCalls, 1)
}

func (f *fakeStuckPendingReactionSource) CaptureDrainSnapshot(context.Context, string) session.DebtSnapshot {
	return session.DebtSnapshot{}
}

func (f *fakeStuckPendingReactionSource) RecordDrainTurnOutcome(context.Context, string, session.DebtSnapshot, error) {
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
