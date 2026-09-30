// B7 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN) and R2B-16: the
// consecutive-auto-turn cap bounds ONLY the SDK background-shell auto-resume
// (claimAutoResume spends one of maxConsecutiveAutoResumes per human message,
// atomically, at submission); the launch policy never re-checks it. Stop's
// (and a pending question's) suspension gates EVERY automatic turn through
// its own state, separate from the counter.
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// newNeverCalledModel returns a probe Model backed by an httptest server
// that fails the test immediately if it is ever hit -- used for a Drain
// scenario expected to take the no-turn branch and never reach a provider.
func newNeverCalledModel(t *testing.T) Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("provider must never be called")
	}))
	t.Cleanup(srv.Close)
	return newProbeModel(t, srv)
}

// TestDrainPolicy_OrdinarySession_Allowed: a plain session with nothing
// suspended is allowed a Drain turn.
func TestDrainPolicy_OrdinarySession_Allowed(t *testing.T) {
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "ordinary-async-wake")
	require.NoError(t, err)

	allowed, err := policyAllowed(coord, ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, allowed)
}

// TestDrainPolicy_StopSuspension_RefusesEveryAutomaticTurn: Stop's suspension
// (suspendAutoResume) refuses a Drain of any origin and never touches the
// bg-shell counter.
//
// Revert-check: removing the autoResumeSuspended check from drainPolicy turns
// the refusal red.
func TestDrainPolicy_StopSuspension_RefusesEveryAutomaticTurn(t *testing.T) {
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "stop-suspended")
	require.NoError(t, err)

	coord.suspendAutoResume(sess.ID)

	v := coord.drainPolicy(ctx, sess.ID)
	require.Equal(t, drainDeferred, v.kind, "Stop's suspension must refuse the turn")
	require.Zero(t, coord.consecutiveResume(sess.ID), "Stop must not consume the bg-shell cap counter")
}

// TestDrainPolicy_BgShellCapExhausted_DoesNotBlockAsyncWake: a full bg-shell
// counter gates only claimAutoResume, never the launch policy: an ordinary
// async-job/delegation/supervision wake was never capped.
//
// Revert-check: re-adding `consecutiveResume < max` to drainPolicy turns the
// assertion red.
func TestDrainPolicy_BgShellCapExhausted_DoesNotBlockAsyncWake(t *testing.T) {
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "cap-vs-async")
	require.NoError(t, err)
	for range maxConsecutiveAutoResumes {
		coord.bumpConsecutiveResume(sess.ID)
	}

	allowed, err := policyAllowed(coord, ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, allowed, "an exhausted bg-shell cap must not pause an ordinary async-job wake")
}

// TestResetAutoResumeCounter_ClearsStopAndCap: the human-message reset lifts
// Stop's suspension AND zeroes the bg-shell counter.
//
// Revert-check: dropping the `delete(c.autoTurnsSuspended, ...)` from
// resetConsecutiveResume turns the policy assertion red; dropping the counter
// delete fails the counter assertion.
func TestResetAutoResumeCounter_ClearsStopAndCap(t *testing.T) {
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "reset-both")
	require.NoError(t, err)
	for range maxConsecutiveAutoResumes {
		coord.bumpConsecutiveResume(sess.ID)
	}
	coord.suspendAutoResume(sess.ID)

	coord.ResetAutoResumeCounter(sess.ID)

	require.Zero(t, coord.consecutiveResume(sess.ID))
	require.False(t, coord.autoResumeSuspended(sess.ID))
	allowed, err := policyAllowed(coord, ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, allowed, "a human message must re-arm automatic turns")
}

// TestWakeSession_BgShellCapExhausted_AsyncJobWakeStillRunsTurn is the real
// wakeSession path: a full bg-shell counter, then an async job completes --
// its reaction turn must still reach the provider.
//
// Revert-check: the same cap re-check in drainPolicy: no request is made.
func TestWakeSession_BgShellCapExhausted_AsyncJobWakeStillRunsTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "cap-exhausted-async-wake", attemptFixtureOpts{noIdle: true})
	for range maxConsecutiveAutoResumes {
		f.coord.bumpConsecutiveResume(f.sessID)
	}
	f.seedDebt(ctx, "call-1", false)

	require.NoError(t, f.coord.wakeSession(ctx, f.sessID, true))
	require.NotZero(t, f.requests.Load(), "an exhausted bg-shell cap must not block an async-job reaction turn")
}
