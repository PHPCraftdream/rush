// Batch translation (H4 lane P5, phase A) of the arbiter-side sweep
// assertions from coordinator_session_state_sweep_test.go (deleted with the
// work ledger). The ledger half of the old tests (l.sweepIdleSessions,
// ledgerEntryIDs, f.ledger.bumpHint) pinned the deleted ledger's idle-entry
// sweep and is intentionally gone; these tests pin what survives in the
// batch world: the arbiter entry sweep (R3B-8, R4B-4) -- the sweep frees
// exactly the entries that hold nothing.
//
// Revert-check: dropping a condition from sweepArbiterEntriesAt frees live
// state and turns an assertion red; dropping the sweep makes "idle" survive.
package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSweepArbiterEntries_DropsOnlyEntriesThatHoldNothing: a paced gate is
// live state; an open zero gate holds nothing and is swept.
func TestSweepArbiterEntries_DropsOnlyEntriesThatHoldNothing(t *testing.T) {
	c := &coordinator{}
	c.arb.pace("paced", 0, time.Hour, false, paceUncounted)
	c.arb.pace("streaky", 0, 0, false, pacePaidUnreacted)
	c.arb.resetGate("streaky") // back to an open gate: idle, swept

	c.sweepArbiterEntries()
	require.Contains(t, c.arb.sessionsWithState(), "paced", "a paced gate is live state")
	require.NotContains(t, c.arb.sessionsWithState(), "streaky", "an open zero gate holds nothing")

	c.arb.resetGate("paced")
	c.sweepArbiterEntries()
	require.NotContains(t, c.arb.sessionsWithState(), "paced")
}

// TestSweepArbiterEntries_ExpiredPauseWithZeroStreaksIsIdle (R4B-4): a gate
// whose pause has PASSED with both streaks at zero is open, like a zero
// gate, and holds nothing: the sweep frees it (a web turn that failed with a
// provider error paces the gate with no debt and no streak; if the session
// is never used again nothing else would). A pause still running, and any
// streak, are live state and stay.
//
// Revert-check: requiring retryAt.IsZero() again keeps the expired entry and
// turns the count assertion red; dropping the streak conditions frees the
// paid/free entries and turns the last two red.
func TestSweepArbiterEntries_ExpiredPauseWithZeroStreaksIsIdle(t *testing.T) {
	now := time.Now()
	c := &coordinator{}
	// The ordinary turn's pause: no debt, no streak. Paced directly on the
	// arbiter: the legacy route (noteTurnFailed via the ledger's arbPace)
	// is gone with the ledger.
	c.arb.pace("failed-turn", 0, drainRetryAfterFailure(), false, paceUncounted)
	c.arb.pace("refused", 0, 30*time.Minute, true, paceUncounted)
	c.arb.pace("paid", 0, 0, false, pacePaidUnreacted) // one paid streak
	c.arb.pace("free", 0, 0, false, paceFreeNoTurn)    // one free streak

	require.Len(t, c.arb.sessionsWithState(), 4, "pauses still running (and streaks) keep their entries")

	// Two hours later the plain pauses are over; the streak entries stay.
	c.sweepArbiterEntriesAt(now.Add(2 * time.Hour))
	alive := c.arb.sessionsWithState()
	require.NotContains(t, alive, "failed-turn", "the failed turn's pause is over")
	require.NotContains(t, alive, "refused", "and the refusal's")
	require.Contains(t, alive, "paid", "a streak is live state")
	require.Contains(t, alive, "free", "a streak is live state")
}
