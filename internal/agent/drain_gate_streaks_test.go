// The launch gate's two dormancy streaks and the per-row close (R3B-4/R3B-5):
// a failing pull never shortens the paid attempt budget, a newer fact reopens
// only a failing pull's dormant gate, and only rows whose OWN counter reached
// K are closed -- a notice that arrived later keeps its own clock. Real SQLite,
// real *sessionAgent, httptest providers; faults are SQLite triggers.
package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

const blockPullTrigger = `CREATE TRIGGER fx_block_pull BEFORE UPDATE OF delivery ON async_jobs
	WHEN NEW.delivery = 'done'
	BEGIN SELECT RAISE(ABORT, 'fx: pull blocked'); END`

// markerTexts is the text of every wake_failed marker written for the session.
func (f *attemptFixture) markerTexts(ctx context.Context) []string {
	f.t.Helper()
	notices, err := f.store.ListSessionNotices(ctx, f.sessID)
	require.NoError(f.t, err)
	var out []string
	for _, no := range notices {
		if no.Kind == session.NoticeKindWakeFailed {
			out = append(out, no.Text)
		}
	}
	return out
}

// R3B-4: one failed pull (free) plus provider failures (paid) must not add up
// to dormancy: the paid budget stays K=3 and the third attempt closes the row.
//
// Revert-check: one shared streak (free and paid outcomes counted together)
// makes the gate dormant after the second provider failure: only 2 requests
// are made, nothing closes and this test goes red.
func TestDrainGate_PullFailureDoesNotShortenThePaidBudget(t *testing.T) {
	ctx := context.Background()
	const retry = 100 * time.Millisecond
	shrinkDrainRetry(t, retry)
	f := newAttemptFixture(t, "gate-mixed-streak", attemptFixtureOpts{handler: emptyReplyResponse})
	f.seedDebt(ctx, "call-1", false)
	f.exec(ctx, blockPullTrigger)

	require.NoError(t, f.wake(ctx, true)) // a no-turn Drain: the pull fails
	require.Zero(t, f.requests.Load())
	f.exec(ctx, `DROP TRIGGER fx_block_pull`)

	for want := int32(1); want <= 3; want++ {
		time.Sleep(retry + 50*time.Millisecond)
		f.pass(ctx)
		require.Equal(t, want, f.requests.Load(), "paid attempt %d must be allowed", want)
	}
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 3, row.WakeAttempts)
	require.EqualValues(t, 1, row.Reacted, "the third paid attempt closes the row")
	require.EqualValues(t, 1, row.ReactedFailed)
	require.Equal(t, 1, f.markers(ctx))
}

// R3B-4: a dormant gate over a pull that keeps failing is reopened by a NEWER
// fact (and only by one: passes launch nothing).
//
// Revert-check: dropping hintOpens from the free pace (or the hint clause from
// the arbiter gate rows) keeps the fourth launch from happening and this test goes red.
func TestDrainGate_FreeDormancyReopensOnNewerFact(t *testing.T) {
	ctx := context.Background()
	const retry = 100 * time.Millisecond
	shrinkDrainRetry(t, retry)
	// No release hook: the test drives every launch itself (a release-launched
	// Drain racing a fact's own would make the launch count nondeterministic).
	f := newAttemptFixture(t, "gate-free-dormant", attemptFixtureOpts{noIdle: true})
	launches := f.countRuns()
	f.seedDebt(ctx, "call-1", false)
	f.exec(ctx, blockPullTrigger)

	for range drainDormantStreak {
		require.NoError(t, f.wake(ctx, true)) // each fact is newer than the last pace
	}
	require.EqualValues(t, drainDormantStreak, launches.runs.Load())
	require.Equal(t, drainStuck, f.coord.drainPermitted(ctx, f.sessID, false).kind, "three no-turn Drains: dormant")

	for range 3 {
		time.Sleep(retry + 50*time.Millisecond)
		f.pass(ctx)
	}
	require.EqualValues(t, drainDormantStreak, launches.runs.Load(), "a pass never reopens a dormant gate")

	require.NoError(t, f.wake(ctx, true))
	require.EqualValues(t, drainDormantStreak+1, launches.runs.Load(), "a newer fact reopens a failing pull's gate")
}

// R3B-4: paid attempts whose close keeps failing make the gate dormant, and a
// newer fact does NOT reopen it (only a human message does).
//
// Revert-check: letting a newer hint open a paid-dormant gate (dropping
// !paidDormant from the hint clause) makes the fact launch a fourth paid
// attempt and this test goes red.
func TestDrainGate_PaidDormancy_NewFactDoesNotReopen(t *testing.T) {
	ctx := context.Background()
	const retry = 100 * time.Millisecond
	shrinkDrainRetry(t, retry)
	f := newAttemptFixture(t, "gate-paid-dormant", attemptFixtureOpts{})
	f.seedDebt(ctx, "call-1", false)
	f.blockReactionsAndSettles(ctx)

	require.NoError(t, f.wake(ctx, true))
	for range 2 {
		time.Sleep(retry + 50*time.Millisecond)
		f.pass(ctx)
	}
	require.EqualValues(t, 3, f.requests.Load(), "three paid attempts, the close keeps failing")
	require.Equal(t, drainStuck, f.coord.drainPermitted(ctx, f.sessID, false).kind)

	f.seedDebt(ctx, "call-2", false)
	require.NoError(t, f.wake(ctx, true))
	require.EqualValues(t, 3, f.requests.Load(), "a newer fact does not reopen a paid-dormant gate")
	// A refusal note (a queued Drain the lock refused) sets hintOpens; the
	// dormant paid gate must not be reopened by the next fact through it.
	f.coord.noteDrainRefused(f.sessID, errors.New("lock busy"))
	f.ledger.bumpHint(f.sessID)
	require.NoError(t, f.wake(ctx, true))
	require.EqualValues(t, 3, f.requests.Load(), "a refusal note does not make a paid-dormant gate hint-openable")

	f.coord.ResetAutoResumeCounter(f.sessID)
	require.NoError(t, f.wake(ctx, true))
	require.EqualValues(t, 4, f.requests.Load(), "a human message reopens it")
}

// R3B-5: when one row reaches K, only rows whose OWN counter reached it are
// closed; a notice that arrived later keeps its own clock (a one-minute
// outage never closes it) and the gate stays paced.
//
// Revert-check: settling the whole snapshot when any row reaches K closes
// call-B with one attempt and this test goes red.
func TestDrainAttempt_OnlyRowsAtKAreClosed(t *testing.T) {
	ctx := context.Background()
	// A long pause the test never waits out: the gate is reopened by hand
	// between runs, and "paced" must not flake into "open" under load.
	shrinkDrainRetry(t, time.Minute)
	f := newAttemptFixture(t, "attempt-per-row-k", attemptFixtureOpts{noIdle: true, handler: emptyReplyResponse})
	f.seedDebt(ctx, "call-A", false)
	for i := int64(1); i <= 2; i++ {
		_, _ = f.drainRun(ctx)
		require.Equal(t, i, f.row(ctx, "call-A").WakeAttempts)
		f.ledger.resetDrainGate(f.sessID) // the retry pause elapses
	}
	f.seedDebt(ctx, "call-B", false) // arrives while A is failing

	_, _ = f.drainRun(ctx) // A reaches K=3, B has its first attempt
	a, b := f.row(ctx, "call-A"), f.row(ctx, "call-B")
	require.EqualValues(t, 3, a.WakeAttempts)
	require.EqualValues(t, 1, a.Reacted, "A used up its attempts")
	require.EqualValues(t, 1, a.ReactedFailed)
	require.EqualValues(t, 1, b.WakeAttempts)
	require.EqualValues(t, 0, b.Reacted, "B keeps its own clock")
	require.EqualValues(t, 0, b.ReactedFailed)
	texts := f.markerTexts(ctx)
	require.Len(t, texts, 1)
	require.Contains(t, texts[0], "call-A")
	require.NotContains(t, texts[0], "call-B", "the marker names only the closed rows")
	require.Equal(t, drainPaced, f.coord.drainPermitted(ctx, f.sessID, false).kind, "rows remain: the gate stays paced")

	for want := int64(2); want <= 3; want++ {
		f.ledger.resetDrainGate(f.sessID) // the retry pause elapses
		_, _ = f.drainRun(ctx)
		require.Equal(t, want, f.row(ctx, "call-B").WakeAttempts)
	}
	require.EqualValues(t, 1, f.row(ctx, "call-B").Reacted, "B closes on ITS third attempt")
	texts = f.markerTexts(ctx)
	require.Len(t, texts, 2)
	require.True(t, strings.Contains(texts[1], "call-B") && !strings.Contains(texts[1], "call-A"))
}
