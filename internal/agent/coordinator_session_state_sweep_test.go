package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func ledgerEntryIDs(l *workLedger) map[string]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]bool, len(l.bySession))
	for id := range l.bySession {
		out[id] = true
	}
	return out
}

// R3B-8: the sweep frees exactly the ledger entries that hold nothing.
//
// Revert-check: dropping any of the four conditions (no jobs, no external
// driver, zero gate) from sweepIdleSessions frees a live entry and turns an
// assertion red; dropping the sweep makes "idle" survive.
func TestSweepIdleSessions_DropsOnlyEntriesThatHoldNothing(t *testing.T) {
	l := newWorkLedger(nil)
	l.bumpHint("idle") // a session that had a fact once, nothing now
	l.bumpHint("paced")
	l.paceDrainGate("paced", l.hintSeqOf("paced"), time.Hour, false, paceUncounted)
	l.bumpHint("streaky")
	l.paceDrainGate("streaky", 0, 0, false, pacePaidUnreacted)
	l.resetDrainGate("streaky") // back to a zero gate: swept
	l.claimExternalDriver("driven")
	l.mu.Lock()
	l.sessionLocked("busy").jobs["j"] = &asyncJob{}
	l.mu.Unlock()

	require.Equal(t, 2, l.sweepIdleSessions(), "idle and streaky (zero gate again) hold nothing")
	ids := ledgerEntryIDs(l)
	require.False(t, ids["idle"])
	require.False(t, ids["streaky"])
	require.True(t, ids["paced"], "a paced gate is live state")
	require.True(t, ids["driven"], "an external driver is live state")
	require.True(t, ids["busy"], "a session with a job is live state")

	l.resetDrainGate("paced")
	require.Equal(t, 1, l.sweepIdleSessions())
	require.False(t, ledgerEntryIDs(l)["paced"])
}

// R3B-8: the 60s pass frees idle ledger entries and the auto-turn state of
// DELETED sessions; Stop's suspension and the cap counter of a session that
// still exists stay (they mean "until a human message").
//
// Revert-check: dropping c.sweepSessionState from RecheckPass leaves the idle
// entry and the deleted session's state and turns the assertions red; sweeping
// counters of live sessions turns the last two red.
func TestRecheckPass_FreesIdleSessionState(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "sweep-live", attemptFixtureOpts{noIdle: true})
	gone, err := f.env.sessions.Create(ctx, "sweep-gone")
	require.NoError(t, err)
	f.coord.sessions = f.env.sessions

	f.ledger.bumpHint("never-had-a-session") // an idle entry, no jobs, zero gate
	f.coord.bumpConsecutiveResume(gone.ID)
	f.coord.autoResumeMu.Lock()
	f.coord.bgShellOverCap = map[string]int{gone.ID: 1, f.sessID: 1}
	f.coord.autoResumeMu.Unlock()
	f.coord.suspendAutoResume(gone.ID)
	f.coord.bumpConsecutiveResume(f.sessID)
	f.coord.suspendAutoResume(f.sessID)
	require.NoError(t, f.env.sessions.Delete(ctx, gone.ID))

	f.pass(ctx)

	require.False(t, ledgerEntryIDs(f.ledger)["never-had-a-session"], "an idle ledger entry is freed")
	require.Zero(t, f.coord.consecutiveResume(gone.ID), "a deleted session's cap counter is freed")
	require.Zero(t, f.coord.bgShellOverCapCount(gone.ID), "a deleted session's over-cap count is freed")
	require.False(t, f.coord.autoResumeSuspended(gone.ID), "a deleted session's suspension is freed")
	require.EqualValues(t, 1, f.coord.consecutiveResume(f.sessID), "a live session keeps its cap counter")
	require.EqualValues(t, 1, f.coord.bgShellOverCapCount(f.sessID), "a live session keeps its over-cap count")
	require.True(t, f.coord.autoResumeSuspended(f.sessID), "a live session keeps Stop's suspension")
}

// R4B-4: a gate whose pause has PASSED with both streaks at zero is open, like a
// zero gate, and holds nothing: the sweep frees it (a web turn that failed with a
// provider error paces the gate with no debt and no streak; if the session is
// never used again nothing else would). A pause still running, and any streak,
// are live state and stay.
//
// Revert-check: requiring retryAt.IsZero() again keeps the expired entry and
// turns the count assertion red; dropping the streak conditions frees the
// paid/free entries and turns the last two red.
func TestSweepIdleSessions_ExpiredPauseWithZeroStreaksIsIdle(t *testing.T) {
	now := time.Now()
	l := newWorkLedger(nil)
	c := &coordinator{asyncJobs: l}
	c.noteTurnFailed("failed-turn") // the ordinary turn's pause: no debt, no streak
	l.paceDrainGate("refused", 0, 30*time.Minute, true, paceUncounted)
	l.paceDrainGate("paid", 0, 0, false, pacePaidUnreacted) // one paid streak
	l.paceDrainGate("free", 0, 0, false, paceFreeNoTurn)    // one free streak

	require.Zero(t, l.sweepIdleSessionsAt(now), "pauses still running (and streaks) keep their entries")

	require.Equal(t, 2, l.sweepIdleSessionsAt(now.Add(2*time.Hour)), "an expired pause with zero streaks holds nothing")
	ids := ledgerEntryIDs(l)
	require.False(t, ids["failed-turn"], "the failed turn's pause is over")
	require.False(t, ids["refused"], "and the refusal's")
	require.True(t, ids["paid"], "a streak is live state")
	require.True(t, ids["free"], "a streak is live state")
}
