// A rerun's hold on automatic turns (R2C-4): no Drain launches while held, and
// the skipped wake is retried by the re-check pass once the last hold is
// released.
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRerunHold_DefersAutomaticTurnsUntilRelease: with a hold taken the wake
// launches nothing (the session stays in the re-check set), a nested hold keeps
// it off until BOTH are released, and the pass after the release runs the turn
// and closes the debt.
//
// Revert-check: dropping the automaticTurnsHeld verdict from drainPolicy lets
// the held wake launch (1 request while held) and this test goes red.
func TestRerunHold_DefersAutomaticTurnsUntilRelease(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "rerun-hold", attemptFixtureOpts{})
	f.seedDebt(ctx, "call-1", false)

	releaseA := f.coord.HoldAutomaticTurns(f.sessID)
	releaseB := f.coord.HoldAutomaticTurns(f.sessID)
	require.NoError(t, f.wake(ctx, true))
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, f.requests.Load(), "no automatic turn while a rerun holds the session")
	require.True(t, f.inRecheckSet(), "the skipped wake is kept for the re-check pass")

	releaseA()
	releaseA() // idempotent: must not release B's hold
	f.pass(ctx)
	require.Zero(t, f.requests.Load(), "holds nest: one release leaves the other in force")

	releaseB()
	f.pass(ctx)
	require.EqualValues(t, 1, f.requests.Load(), "the pass after the last release runs the turn")
	require.EqualValues(t, 1, f.row(ctx, "call-1").Reacted)
	require.False(t, f.coord.automaticTurnsHeld(f.sessID))
}
