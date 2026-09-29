// B8/C5b,c (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): the
// CLI loop's waitForNextCLITurn must decide "another turn is owed" from the
// SAME predicate decideDrainTurn uses to decide "a turn actually runs"
// (VISIBLE debt, delivery='done') -- not the pending-inclusive
// ReactionDebtExists. Before this fix, a permanently-pending row (a stuck
// pull) made waitForNextCLITurn return hasNext=true immediately, forever,
// without ever calling WaitForHint -- a 100%-CPU tight loop that never
// exits, since every such "turn" actually taken by decideDrainTurn's own
// visible-only check silently no-ops (ErrRunQueued).
package app

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// fakeStuckPendingReactionSource simulates a session whose debt row is
// permanently stuck at delivery='pending' (VisibleReactionDebtExists always
// false) while the plain, pending-inclusive predicate always reports debt
// present -- exactly what a permanently failing pull leaves behind.
// WaitForHint cancels ctx on its first call so the test terminates
// deterministically instead of hanging: production's real WaitForHint has
// its own bounded fallback (reactionDebtSourceHintFallback) and the 60s
// pass, neither of which this fake needs to reproduce for THIS test's
// purpose (which is "did the loop reach the wait step at all", not how long
// it waits).
type fakeStuckPendingReactionSource struct {
	visibleDebtCalls int32
	plainDebtCalls   int32
	waitForHintCalls int32
	cancel           context.CancelFunc
}

func (f *fakeStuckPendingReactionSource) ClaimExternalDriver(string)          {}
func (f *fakeStuckPendingReactionSource) ReleaseExternalDriver(string)        {}
func (f *fakeStuckPendingReactionSource) RunMaintenanceSweep(context.Context) {}

func (f *fakeStuckPendingReactionSource) ReactionDebtExists(context.Context, string) (bool, error) {
	atomic.AddInt32(&f.plainDebtCalls, 1)
	return true, nil // pending-inclusive: the stuck row still counts
}

func (f *fakeStuckPendingReactionSource) VisibleReactionDebtExists(context.Context, string) (bool, error) {
	atomic.AddInt32(&f.visibleDebtCalls, 1)
	return false, nil // never visible -- the pull never succeeds
}

func (f *fakeStuckPendingReactionSource) ScopeOpen(context.Context, string) (bool, error) {
	return true, nil // ScopeOpen's own internal check is also pending-inclusive
}

func (f *fakeStuckPendingReactionSource) WaitForHint(context.Context, string) {
	atomic.AddInt32(&f.waitForHintCalls, 1)
	if f.cancel != nil {
		f.cancel()
	}
}

func (f *fakeStuckPendingReactionSource) CaptureDrainSnapshot(context.Context, string) session.DebtSnapshot {
	return session.DebtSnapshot{}
}

func (f *fakeStuckPendingReactionSource) RecordDrainTurnOutcome(context.Context, string, session.DebtSnapshot, error) {
}

// TestWaitForNextCLITurn_StuckPendingDebt_WaitsInsteadOfSpinning is the
// regression test.
//
// Revert-check performed: changed waitForNextCLITurn's first debt read back
// to source.ReactionDebtExists (the pending-inclusive predicate) -- this
// test FAILED: hasNext was true, err was nil (not context.Canceled), and
// waitForHintCalls stayed 0 -- the function returned hasNext=true on its
// very first iteration without ever reaching the wait step, exactly the
// pre-fix tight-loop shape. Restored VisibleReactionDebtExists; re-ran,
// passed.
func TestWaitForNextCLITurn_StuckPendingDebt_WaitsInsteadOfSpinning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeStuckPendingReactionSource{}
	f.cancel = cancel

	app := &App{}
	hasNext, err := app.waitForNextCLITurn(ctx, f, "sess-1")

	require.False(t, hasNext, "a permanently-pending (not visible) row must never report a turn as owed")
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, atomic.LoadInt32(&f.visibleDebtCalls), int32(1),
		"the loop must consult the VISIBLE debt predicate, matching decideDrainTurn")
	require.GreaterOrEqual(t, atomic.LoadInt32(&f.waitForHintCalls), int32(1),
		"the loop must reach the wait step instead of returning hasNext=true immediately")
}
