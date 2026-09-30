// Step-4 follow-up (Ф4-4 review, doc sec.6): "an orphaned Drain (the row
// arrived in the release window) does not lose the wake". The existing debt
// tests (coordinator_wake_reaction_debt_test.go) deliberately wire NO idle
// hook for the tests that drive their own two sequential wakeSession calls,
// precisely because abandonOwnershipWithHandoff drops an orphaned Drain
// outright by design (agent_ownership.go: "dropping an orphaned Drain here
// is always safe" since a later wake re-derives it). This file is that
// "later wake": it proves the real production convergence path -- the
// release hook's OWN separate-goroutine debt re-check (onSessionIdleHook ->
// afterRelease, supervision.go) -- actually closes the gap the drop
// opens, using the REAL OnSessionIdle hook (newWakeDebtFixture, not the
// NoIdleHook variant).
package agent

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestOrphanedDrain_ReleaseTimeRecheckConverges reproduces doc sec.6's
// scenario end to end:
//
//  1. A job row becomes debt (a real store.Transition with wake=1) at
//     EXACTLY the moment a Drain call races into the mailbox during the
//     release() window -- the same shape run_orphaned_release_race_test.go
//     uses for its own raced-call proof, here with a Drain instead of an
//     ordinary prompt call, and with the race committing the debt row too.
//  2. That orphaned Drain is proven to be dropped outright: restarting it
//     (exactly what drainOrReleaseMerged/abandonOwnershipWithHandoff do with
//     an `orphaned`/`popped` list) produces NO provider call, ever.
//  3. The release hook's real OnSessionIdle -> onSessionIdleHook ->
//     afterRelease chain (fired the same way abandonOwnershipWithHandoff
//     fires it on every real Run() exit) independently discovers the SAME
//     debt from the DB and submits a fresh Drain, which DOES run: exactly
//     one provider request, and the debt ends up reacted.
//
// REVERT CHECK: commented out the `go c.afterRelease(sessionID)`
// call in onSessionIdleHook (supervision.go), leaving every other line
// intact -- this test's final `require.Eventually` (provider request count
// becoming nonzero) timed out and failed, exactly as expected: with the
// re-check disabled, the dropped orphaned Drain's wake is genuinely lost
// forever, nothing else in the system ever discovers this debt again.
// Restored the call (byte-identical diff confirmed against a saved copy);
// re-ran, passed.
func TestOrphanedDrain_ReleaseTimeRecheckConverges(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newWakeDebtFixture(t, "orphaned-drain-converges")

	// A running job, claimed and announced but NOT yet terminal -- the
	// memory-side bookkeeping claimAndFinish also does, minus the terminal
	// transition itself (that happens inside raceRelease below, "during the
	// release window").
	_, existing, err := f.ledger.Start(f.sessID, "call-1", "call-1", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)
	require.NoError(t, f.store.MarkAnnounced(ctx, f.sessID, "call-1"))

	// Hand-craft the mailbox into "mid-turn" (mbOwned, epoch 1) exactly like
	// run_orphaned_release_race_test.go's own full-path test, so
	// drainOrReleaseFinal can be driven directly without a real Run() in
	// flight.
	mb := f.sa.getMailbox(f.sessID)
	mb.mu.Lock()
	mb.state = mbOwned
	mb.epoch = 1
	mb.dispatcherCancel = func() {}
	mb.current = generation{id: 1, cancel: func() {}}
	mb.mu.Unlock()

	// raceRelease fires INSIDE drainOrReleaseFinal's release window: the
	// job's terminal transition commits its debt (wake=1, delivery=
	// 'pending') at the exact moment a Drain call races into the
	// now-being-vacated mailbox.
	raceRelease := func() error {
		_, err := f.store.Transition(ctx, session.TransitionParams{
			Owner: f.sessID, ToolCallID: "call-1", State: "completed",
			ResultSummary: "output", Wake: true,
		})
		require.NoError(t, err)
		mb.mu.Lock()
		mb.submitted = append(mb.submitted, newDrainCall(SessionAgentCall{SessionID: f.sessID}))
		mb.mu.Unlock()
		return nil
	}

	next, hasNext, orphaned, releaseErr := mb.drainOrReleaseFinal(1, raceRelease)
	require.NoError(t, releaseErr)
	require.False(t, hasNext, "the original caller must not be told to keep running -- the mailbox is already released")
	require.Equal(t, SessionAgentCall{}, next)
	require.Len(t, orphaned, 1, "the Drain that raced in during release() must be reported as orphaned")
	require.True(t, orphaned[0].IsDrain, "the orphaned call must be the Drain this test seeded")
	// The plain (not Visible-only) predicate: the row is still
	// delivery='pending' at this point (nothing has pulled it into history
	// yet) -- exactly the shape afterRelease itself reads.
	debtNow, err := f.store.ReactionDebtExists(ctx, f.sessID)
	require.NoError(t, err)
	require.True(t, debtNow, "the race's own transition must have made the debt real before anything else runs")

	// Exactly what drainOrReleaseMerged does with drainOrReleaseFinal's own
	// `orphaned` return: restart it. For a Drain, agent_ownership.go's
	// restartOrphanedWithRetry drops it outright -- no goroutine ever
	// touches the provider for it.
	f.sa.restartOrphaned(orphaned)
	require.Never(t, func() bool { return f.requests.Load() > 0 }, 250*time.Millisecond, 10*time.Millisecond,
		"an orphaned Drain must be dropped outright and must never itself reach the provider")

	// Production's real finalizer (runOwned's defer, agent_run.go) always
	// calls abandonOwnershipWithHandoff on every Run() exit, which always
	// fires onSessionIdle -- do the same here, directly, against the REAL
	// hook this fixture wired (newWakeDebtFixture, not NoIdleHook).
	f.coord.onSessionIdleHook(f.sessID)

	require.Eventually(t, func() bool { return f.requests.Load() > 0 }, 2*time.Second, 10*time.Millisecond,
		"the release-time recheck must independently discover the same debt and submit a fresh Drain that runs")
	require.EqualValues(t, 1, f.requests.Load(), "exactly one provider turn must result -- no loss, no loop")
	require.Eventually(t, func() bool { return !f.debtVisible(t, ctx) }, 2*time.Second, 10*time.Millisecond,
		"the fresh Drain's own turn must react to the notice and clear the debt")
}
