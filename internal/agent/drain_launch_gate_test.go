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

// elapseDrainPause checks both sides of the exact recorded pacing boundary
// with the arbiter's input clock, then expires only RetryAt for the real pass.
// Streaks and hint state survive; resetting the gate would erase the budget.
func (f *attemptFixture) elapseDrainPause(ctx context.Context) {
	f.t.Helper()
	facts, err := f.coord.readTurnFacts(ctx, f.sessID, siteTick)
	require.NoError(f.t, err)
	require.True(f.t, facts.Gate.Paced)
	facts.Now = facts.Gate.RetryAt.Add(-time.Nanosecond)
	v, shut := gateShut(facts)
	require.True(f.t, shut)
	require.Equal(f.t, VDefer, v.Kind)
	facts.Now = facts.Gate.RetryAt
	_, shut = gateShut(facts)
	require.False(f.t, shut, "the pause ends at RetryAt, not later")
	f.coord.arb.mu.Lock()
	f.coord.arb.stateLocked(f.sessID).gate.RetryAt = time.Unix(1, 0)
	f.coord.arb.mu.Unlock()
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
	const retry = time.Hour
	shrinkDrainRetry(t, retry)
	f := newAttemptFixture(t, "attempt-paced", attemptFixtureOpts{noIdle: true, handler: emptyReplyResponse})
	f.seedDebt(ctx, "call-1", false)

	require.NoError(t, f.wake(ctx, true))
	require.EqualValues(t, 1, f.row(ctx, "call-1").WakeAttempts)
	f.coord.afterRelease(f.sessID)
	require.EqualValues(t, 1, f.requests.Load(), "the release of an unreacted attempt must not relaunch a paid turn")

	for want := int32(2); want <= 3; want++ {
		f.elapseDrainPause(ctx)
		f.pass(ctx)
		f.coord.afterRelease(f.sessID)
		require.Equal(t, want, f.requests.Load(), "the gate reopens for one attempt per pause")
	}
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 3, row.WakeAttempts)
	require.EqualValues(t, 1, row.Reacted, "K=3 closes the debt")
	require.EqualValues(t, 1, row.ReactedFailed)
	require.Equal(t, 1, f.markers(ctx))

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
	// Under the batch runtime the release path can tear the driver down (see
	// ensureDrainDriver); the final wake below must take the driver branch
	// (the root branch dereferences the fixture's nil cfg). The wake above
	// stays gated by the suspension either way, so re-registering here does
	// not disturb the no-turn assertion it precedes.
	f.ensureDrainDriver()
	require.NoError(t, f.wake(ctx, true))
	f.waitReleases() // no wait-out needed: the gate is suspension-, not pacing-, shut
	require.EqualValues(t, 1, f.requests.Load(), "no automatic turn while the question waits")

	f.coord.ResetAutoResumeCounter(f.sessID)
	f.ensureDrainDriver()
	require.ErrorAs(t, f.wake(ctx, true), &awaiting)
	f.waitReleases()
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
	f := newAttemptFixture(t, "attempt-dormant", attemptFixtureOpts{manualClock: true})
	f.seedDebt(ctx, "call-1", false)
	f.blockReactionsAndSettles(ctx)
	require.NoError(t, f.wake(ctx, true))
	for range 6 {
		f.waitReleases() // the attempt must finish and pace the gate first
		f.advanceDrainClock(retry + time.Millisecond)
		f.pass(ctx)
	}
	f.waitReleases()

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
	f := newAttemptFixture(t, "attempt-pull-fail", attemptFixtureOpts{manualClock: true})
	launches := f.countRuns()
	f.seedDebt(ctx, "call-1", false)
	a1BlockPull(t, f.env.conn)

	require.NoError(t, f.wake(ctx, true))
	for range 6 {
		f.waitReleases() // the attempt must finish and pace the gate first
		f.advanceDrainClock(retry + time.Millisecond)
		f.pass(ctx)
	}
	f.waitReleases()

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
	f.waitReleases() // the wake is synchronous; the wait only guards the counter read

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
	recheckDebtCheckBudget = 2 * time.Second
	t.Cleanup(func() { recheckDebtCheckBudget = old })
	f := newAttemptFixture(t, "attempt-hook-db", attemptFixtureOpts{noIdle: true})
	plain, err := f.env.sessions.Create(ctx, "a plain session, never a delegation driver")
	require.NoError(t, err)
	require.False(t, f.ledger.hasArmedOrDriver(plain.ID), "precondition: nothing armed, no driver")
	conn, err := f.env.conn.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
	require.NoError(t, err)

	// This test asserts NON-BLOCKING wall-clock behavior and must stay real-time.
	start := time.Now()
	f.coord.onSessionIdleHook(plain.ID)
	elapsed := time.Since(start)

	require.Less(t, elapsed, 100*time.Millisecond, "the hook must return at once")
	// The DB stays held until the off-thread debt read has run into its budget:
	// it fails and keeps the session for the tick.
	require.Eventually(t, func() bool {
		f.coord.recheckMu.Lock()
		defer f.coord.recheckMu.Unlock()
		_, queued := f.coord.recheckSet[plain.ID]
		return queued
	}, 15*time.Second, 10*time.Millisecond, "the blocked debt read ends at its budget, off the hook's goroutine, and keeps the session for the tick")
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
			// Dual-mode injector. OFF: the delegation-state input is the
			// async_jobs row, renamed here. ON: this case can no longer isolate
			// the delegation-state input, and that is a recorded limitation:
			// every ON facts input is read from the ONE batch_nodes source
			// through one binding (batch_cutover_facts.go: PendingInclusiveDebt
			// is read first at :496, the delegation-state read
			// LiveDelegationChildOf only runs later at :535 and only for a
			// session with a parent), and SQLite offers no read-failure seam --
			// a table rename fails the earlier debt read first. A child-session
			// fixture would additionally change the policy context the other
			// three cases pin. What stays pinned under ON is the fail-closed
			// contract itself: deferred + recheck + error + the shared
			// "launch decision input unreadable" reason.
			if batchFlagEnabled() {
				f.exec(ctx, `ALTER TABLE batch_nodes RENAME TO fx_batch_nodes`)
				return
			}
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
