// Drain launch decisions (docs/reviews/2026-09-30-async-phase4-round2-
// attempts-design.md sec.1.5-1.7): every launch reads ONE predicate --
// policy, then the per-session gate the attempt accounting writes. Real
// SQLite, real *sessionAgent, httptest providers; faults are SQLite triggers.
package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// policyAllowed reports the policy half of the launch predicate. The policy
// refuses (or allows) a session regardless of debt -- only the LAUNCHERS
// pre-check the debt -- so nothing is seeded here.
func policyAllowed(c *coordinator, ctx context.Context, sessionID string) (bool, error) {
	v := c.drainPolicy(ctx, sessionID, false)
	return v.kind == drainAllow, v.err
}

func (f *attemptFixture) wake(ctx context.Context, fact bool) error {
	return f.coord.wakeSession(ctx, f.sessID, fact)
}

// pass runs one forced re-check pass and waits for its detached wakes.
func (f *attemptFixture) pass(ctx context.Context) {
	f.coord.RecheckPass(ctx)
	f.coord.waitRecheckWakes()
}

// shrinkDrainRetry shrinks the post-failure pacing for the calling test. The
// calling test must not be parallel (the pacing is process-wide).
func shrinkDrainRetry(t *testing.T, d time.Duration) {
	t.Helper()
	old := drainRetryAfterNS.Swap(int64(d))
	t.Cleanup(func() { drainRetryAfterNS.Store(old) })
}

// countingAgent counts the Run calls (Drain launches) a session's driver gets.
type countingAgent struct {
	SessionAgent
	runs atomic.Int32
}

func (c *countingAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	c.runs.Add(1)
	return c.SessionAgent.Run(ctx, call)
}

func (f *attemptFixture) countRuns() *countingAgent {
	ca := &countingAgent{SessionAgent: f.sa}
	f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{agent: ca, call: SessionAgentCall{SessionID: f.sessID}})
	return ca
}

// A2: an empty stream (err==nil, nothing reacted) is a counted attempt; its
// own release does not relaunch; the re-check passes retry it at the gate's
// pace and the third counted attempt closes the debt with one marker.
//
// Revert-check: making drainPermitted return the policy verdict only (no
// gate) lets the release hook relaunch at once: the request count after the
// first attempt exceeds 1 and this test goes red.
func TestDrainAttempt_EmptyStreamPacedThenSettledAtK(t *testing.T) {
	ctx := context.Background()
	const retry = 300 * time.Millisecond
	shrinkDrainRetry(t, retry)
	f := newAttemptFixture(t, "attempt-paced", attemptFixtureOpts{handler: emptyReplyResponse})
	f.seedDebt(ctx, "call-1", false)

	require.NoError(t, f.wake(ctx, true))
	require.EqualValues(t, 1, f.row(ctx, "call-1").WakeAttempts)
	time.Sleep(retry / 2)
	require.EqualValues(t, 1, f.requests.Load(), "the release of an unreacted attempt must not relaunch a paid turn")

	for want := int32(2); want <= 3; want++ {
		time.Sleep(retry)
		f.pass(ctx)
		require.Equal(t, want, f.requests.Load(), "the gate reopens for one attempt per pause")
	}
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 3, row.WakeAttempts)
	require.EqualValues(t, 1, row.Reacted, "K=3 closes the debt")
	require.EqualValues(t, 1, row.ReactedFailed)
	require.Equal(t, 1, f.markers(ctx))

	time.Sleep(retry)
	f.pass(ctx)
	require.EqualValues(t, 3, f.requests.Load(), "a closed debt launches nothing")
}

// A4: a question asked in a Drain turn is the reaction and suspends automatic
// turns until a human message; a later fact makes no request until then.
//
// Revert-check: dropping the suspension in afterTurn lets the second fact
// launch (2 requests before the reset); dropping the awaiting reaction write
// leaves the first row unreacted. Either turns this test red.
func TestDrainAttempt_AskQuestionReactsAndSuspends(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-question", attemptFixtureOpts{
		tools: []fantasy.AgentTool{tools.NewAskQuestionTool()}, handler: askQuestionResponse,
	})
	f.seedDebt(ctx, "call-1", false)

	err := f.wake(ctx, true)
	var awaiting *AwaitingAnswerError
	require.ErrorAs(t, err, &awaiting)
	require.EqualValues(t, 1, f.row(ctx, "call-1").Reacted, "the question is the reaction")
	require.True(t, f.coord.autoResumeSuspended(f.sessID))

	f.seedDebt(ctx, "call-2", false)
	require.NoError(t, f.wake(ctx, true))
	time.Sleep(300 * time.Millisecond)
	require.EqualValues(t, 1, f.requests.Load(), "no automatic turn while the question waits")

	f.coord.ResetAutoResumeCounter(f.sessID)
	require.ErrorAs(t, f.wake(ctx, true), &awaiting)
	require.EqualValues(t, 2, f.requests.Load(), "a human message resumes automatic turns")
}

// A6: the reaction write AND the settle both fail: three counted attempts,
// then the gate is dormant -- no further paid attempt however many passes run.
//
// Revert-check: removing the dormant streak from the arbiter gate rows lets every
// pass launch again (6 more requests) and this test goes red.
func TestDrainAttempt_ReactionAndSettleFail_DormantAfterThree(t *testing.T) {
	ctx := context.Background()
	const retry = 100 * time.Millisecond
	shrinkDrainRetry(t, retry)
	f := newAttemptFixture(t, "attempt-dormant", attemptFixtureOpts{})
	f.seedDebt(ctx, "call-1", false)
	f.blockReactionsAndSettles(ctx)

	require.NoError(t, f.wake(ctx, true))
	for range 6 {
		time.Sleep(retry + 50*time.Millisecond)
		f.pass(ctx)
	}

	require.EqualValues(t, 3, f.requests.Load(), "three counted attempts, then dormant")
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 3, row.WakeAttempts)
	require.EqualValues(t, 0, row.Reacted)
	require.Zero(t, f.markers(ctx))
}

// A7: a pull that never succeeds leaves the row pending: every Drain is a
// no-turn leg, the provider is never called, and the launches are bounded
// (three, then dormant).
//
// Revert-check: removing the dormant streak launches on every pass (7
// launches) and this test goes red.
func TestDrainAttempt_PermanentPullFailure_BoundedNoTurns(t *testing.T) {
	ctx := context.Background()
	const retry = 100 * time.Millisecond
	shrinkDrainRetry(t, retry)
	f := newAttemptFixture(t, "attempt-pull-fail", attemptFixtureOpts{})
	launches := f.countRuns()
	f.seedDebt(ctx, "call-1", false)
	f.exec(ctx, `CREATE TRIGGER fx_block_pull BEFORE UPDATE OF delivery ON async_jobs
		WHEN NEW.delivery = 'done'
		BEGIN SELECT RAISE(ABORT, 'fx: pull blocked'); END`)

	require.NoError(t, f.wake(ctx, true))
	for range 6 {
		time.Sleep(retry + 50*time.Millisecond)
		f.pass(ctx)
	}

	require.Zero(t, f.requests.Load(), "a pull that never succeeds never reaches the provider")
	require.EqualValues(t, 3, launches.runs.Load(), "bounded no-turn launches")
	require.Equal(t, "pending", f.row(ctx, "call-1").Delivery)
}

// A11: ANY refusal before the turn loop (here: the lock directory cannot be
// created -- not a "busy" lock) paces the gate instead of hot-looping the
// release re-check.
//
// Revert-check: dropping noteRefusal from the lock-error path makes the
// release hook relaunch endlessly (hundreds of Run calls) and this test goes
// red.
func TestAdmissionRefusal_NonBusyLockError_NoHotLoop(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-lock-error", attemptFixtureOpts{})
	launches := f.countRuns()
	f.seedDebt(ctx, "call-1", false)
	require.NoError(t, os.WriteFile(filepath.Join(f.env.workingDir, "locks"), []byte("not a directory"), 0o644))

	_ = f.wake(ctx, true)
	time.Sleep(500 * time.Millisecond)

	require.EqualValues(t, 1, launches.runs.Load(), "a refused launch must not relaunch itself")
	require.True(t, f.inRecheckSet(), "the refusal keeps the wake for the next tick")
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 0, row.WakeAttempts, "a refusal is never counted")
	require.EqualValues(t, 0, row.Reacted)
	require.Zero(t, f.markers(ctx))
	require.Zero(t, f.requests.Load())
}

// A12: the release hook does no work on the releasing goroutine: with the DB
// held by someone else it still returns at once, for a PLAIN session (never a
// delegation driver), whose afterRelease does a bounded debt read -- the work
// the hook would otherwise do inline -- and still does it, off-thread.
//
// Revert-check: running afterRelease synchronously in the hook blocks for the
// (shrunk) decision budget and this test goes red.
func TestReleaseHook_NonChildSession_DoesNotBlockOnDB(t *testing.T) {
	ctx := context.Background()
	old := recheckDebtCheckBudget
	recheckDebtCheckBudget = 300 * time.Millisecond
	t.Cleanup(func() { recheckDebtCheckBudget = old })
	f := newAttemptFixture(t, "attempt-hook-db", attemptFixtureOpts{noIdle: true})
	plain, err := f.env.sessions.Create(ctx, "a plain session, never a delegation driver")
	require.NoError(t, err)
	require.False(t, f.ledger.hasArmedOrDriver(plain.ID), "precondition: nothing armed, no driver")
	conn, err := f.env.conn.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
	require.NoError(t, err)

	start := time.Now()
	f.coord.onSessionIdleHook(plain.ID)
	elapsed := time.Since(start)

	require.Less(t, elapsed, 50*time.Millisecond, "the hook must return at once")
	// The DB stays held until the off-thread debt read has run into its budget:
	// it fails and keeps the session for the tick.
	require.Eventually(t, func() bool {
		f.coord.recheckMu.Lock()
		defer f.coord.recheckMu.Unlock()
		_, queued := f.coord.recheckSet[plain.ID]
		return queued
	}, 5*time.Second, 10*time.Millisecond, "the blocked debt read ends at its budget, off the hook's goroutine, and keeps the session for the tick")
	_, _ = conn.ExecContext(ctx, "ROLLBACK")
	require.NoError(t, conn.Close())
}

// failingGetSessions makes Get fail for one session id: the ONE read
// isDurableDelegationChild makes of the sessions table.
type failingGetSessions struct {
	session.Service
	failID string
}

func (s failingGetSessions) Get(ctx context.Context, id string) (session.Session, error) {
	if id == s.failID {
		return session.Session{}, errors.New("fx: sessions read failed")
	}
	return s.Service.Get(ctx, id)
}

// A14: an unreadable policy input fails CLOSED -- deferred, asking for a
// re-check tick -- and every input is pinned on its OWN read: each case fails
// exactly one read (a renamed table, or a failing Get) and the verdict names
// that input. (A whole-DB close fails the first read only, so the later inputs,
// the delegation-identity read a released child needs included, were never
// reached.)
//
// Revert-check: answering "allow" for an unreadable input (the old fail-open)
// turns every case red; dropping the error branch of any single read turns that
// case red.
func TestDrainPolicy_ReadErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	// R-ARB-2: the facts are ONE snapshot, so an unreadable input no longer
	// names the single read that failed (the old per-input labels); the
	// verdict still fails CLOSED for every input, and each case still fails
	// exactly one read.
	cases := []struct {
		name, reason string
		fail         func(f *attemptFixture)
	}{
		{"driver marker", "launch decision input unreadable", func(f *attemptFixture) {
			f.exec(ctx, `ALTER TABLE session_drivers RENAME TO fx_session_drivers`)
		}},
		{"delegation state", "launch decision input unreadable", func(f *attemptFixture) {
			f.exec(ctx, `ALTER TABLE async_jobs RENAME TO fx_async_jobs`)
		}},
		{"delegation identity", "launch decision input unreadable", func(f *attemptFixture) {
			f.coord.sessions = failingGetSessions{Service: f.env.sessions, failID: f.sessID}
		}},
		{"debt kinds", "launch decision input unreadable", func(f *attemptFixture) {
			f.exec(ctx, `ALTER TABLE session_notices RENAME TO fx_session_notices`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAttemptFixture(t, "attempt-policy-readerr", attemptFixtureOpts{noIdle: true})
			f.seedDebt(ctx, "call-1", false)
			tc.fail(f)

			v := f.coord.drainPolicy(ctx, f.sessID, false)

			require.Equal(t, drainDeferred, v.kind, "an unreadable policy input must never allow a Drain")
			require.True(t, v.recheck, "and asks for a re-check tick")
			require.Error(t, v.err)
			require.Equal(t, tc.reason, v.reason, "the verdict names the input whose read failed")
		})
	}
}
