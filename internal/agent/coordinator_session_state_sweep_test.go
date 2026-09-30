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
	f.coord.suspendAutoResume(gone.ID)
	f.coord.bumpConsecutiveResume(f.sessID)
	f.coord.suspendAutoResume(f.sessID)
	require.NoError(t, f.env.sessions.Delete(ctx, gone.ID))

	f.pass(ctx)

	require.False(t, ledgerEntryIDs(f.ledger)["never-had-a-session"], "an idle ledger entry is freed")
	require.Zero(t, f.coord.consecutiveResume(gone.ID), "a deleted session's cap counter is freed")
	require.False(t, f.coord.autoResumeSuspended(gone.ID), "a deleted session's suspension is freed")
	require.EqualValues(t, 1, f.coord.consecutiveResume(f.sessID), "a live session keeps its cap counter")
	require.True(t, f.coord.autoResumeSuspended(f.sessID), "a live session keeps Stop's suspension")
}
