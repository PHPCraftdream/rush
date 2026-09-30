// Drain launch decisions (docs/reviews/2026-09-30-async-phase4-round2-
// attempts-design.md sec.1.5-1.7): every launch reads ONE predicate --
// policy, then the per-session gate the attempt accounting writes. Real
// SQLite, real *sessionAgent, httptest providers; faults are SQLite triggers.
package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// policyAllowed reports the policy half of the launch predicate.
func policyAllowed(c *coordinator, ctx context.Context, sessionID string) (bool, error) {
	v := c.drainPolicy(ctx, sessionID)
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
// Revert-check: removing the dormant streak from drainGateOpen lets every
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
// writer held by someone else it still returns at once.
//
// Revert-check: running afterRelease synchronously in the hook blocks until
// the (shrunk) decision budget and this test goes red.
func TestReleaseHook_NonChildSession_DoesNotBlockOnDB(t *testing.T) {
	ctx := context.Background()
	old := recheckDebtCheckBudget
	recheckDebtCheckBudget = 300 * time.Millisecond
	t.Cleanup(func() { recheckDebtCheckBudget = old })
	f := newAttemptFixture(t, "attempt-hook-db", attemptFixtureOpts{noIdle: true})
	conn, err := f.env.conn.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
	require.NoError(t, err)

	start := time.Now()
	f.coord.onSessionIdleHook(f.sessID)
	elapsed := time.Since(start)

	_, _ = conn.ExecContext(ctx, "ROLLBACK")
	require.NoError(t, conn.Close())
	require.Less(t, elapsed, 50*time.Millisecond, "the hook must return at once")
}

// A13: six completions on a web session with auto-resume on submit exactly
// maxConsecutiveAutoResumes Drains per human message; the counter is spent
// once, atomically, and never re-checked downstream.
//
// Revert-check: re-adding the cap comparison to drainPolicy (the old "<" after
// the bump) refuses the fifth submission (4 launches) and this test goes red.
func TestBGShellCap_ExactlyFiveAutoResumes(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-bgshell-cap", attemptFixtureOpts{noIdle: true})
	cfg, err := config.Init(f.env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Options = &config.Options{AutoResumeOnJobDone: boolPtr(true)}
	f.coord.cfg = cfg
	f.coord.SetPersistentMode(true)
	mock := &mockSessionAgent{}
	var launches atomic.Int32
	mock.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		launches.Add(1)
		return nil, nil
	}
	f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{agent: mock, call: SessionAgentCall{SessionID: f.sessID}})

	submitted := 0
	for i := range maxConsecutiveAutoResumes + 1 {
		require.NoError(t, f.store.InsertSessionNotice(ctx, f.sessID, "bg_shell_done", "job finished", true, ""))
		if f.coord.claimAutoResume(f.sessID) {
			submitted++
			require.NoError(t, f.coord.wakeSession(ctx, f.sessID, true), "completion %d", i)
		}
	}
	require.Equal(t, maxConsecutiveAutoResumes, submitted)
	require.EqualValues(t, maxConsecutiveAutoResumes, launches.Load(), "exactly five Drains per human message")

	f.coord.ResetAutoResumeCounter(f.sessID)
	require.True(t, f.coord.claimAutoResume(f.sessID), "a human message re-arms auto-resume")
}

// A14: an unreadable policy input fails CLOSED for every session -- a released
// delegation child included -- and asks for a re-check tick.
//
// Revert-check: answering "allow" on a read error (the old fail-open) turns
// both assertions red.
func TestDrainPolicy_ReadErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-policy-readerr", attemptFixtureOpts{noIdle: true})
	require.NoError(t, f.env.conn.Close())

	v := f.coord.drainPolicy(ctx, f.sessID)
	require.Equal(t, drainDeferred, v.kind)
	require.True(t, v.recheck)
	require.Error(t, v.err)

	child := "some-child-session"
	v = f.coord.drainPolicy(ctx, child)
	require.Equal(t, drainDeferred, v.kind, "an unreadable policy must never allow a Drain, whatever the session")
	require.True(t, v.recheck)
}
