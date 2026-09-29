// B7 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): the
// consecutive-auto-turn cap. Design decision (operator HARD RULES item 1):
// async-job/delegation notice wakes do NOT count toward (or get throttled
// by) the cap, matching pre-phase-4 behavior -- but Stop's own "automatic
// turns paused until the next human message" (suspendAutoResume, which
// deliberately reuses this SAME counter rather than a second flag) must
// still gate them. Only the SDK background-shell auto-resume path keeps the
// cap's THROTTLE behavior. Separately: whichever category IS counted must
// count only a Drain that actually reached the provider, never one that was
// merely queued or took the no-turn branch.
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
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

// TestSessionDrainPolicy_OrdinaryAsyncWake_UncountedByDefault: a plain ctx
// (no capAlreadyCountedCtxKey, i.e. any wake origin other than
// notifyBackgroundJobDone) is allowed and NOT counted.
func TestSessionDrainPolicy_OrdinaryAsyncWake_UncountedByDefault(t *testing.T) {
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "ordinary-async-wake")
	require.NoError(t, err)

	allowed, counted, err := coord.sessionDrainPolicy(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, allowed)
	require.False(t, counted, "an ordinary async-job/delegation wake must not count toward the web auto-turn cap")
}

// TestSessionDrainPolicy_OrdinaryAsyncWake_StillRefusedAfterStopSuspension
// proves the uncapped category does NOT escape Stop's own suspension:
// suspendAutoResume (Cancel) forces the SAME counter to the cap, and a
// plain-ctx (uncounted-category) check must still see that and refuse.
//
// Revert-check performed: changed the uncapped branch to `return true,
// false, nil` unconditionally (ignoring consecutiveResume entirely) --
// this test's `require.False(t, allowed)` FAILED (allowed was true even
// though the session had just been Stop-suspended). Restored the
// consecutiveResume read; re-ran, passed.
func TestSessionDrainPolicy_OrdinaryAsyncWake_StillRefusedAfterStopSuspension(t *testing.T) {
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "stop-suspended-async-wake")
	require.NoError(t, err)

	coord.suspendAutoResume(sess.ID)

	allowed, counted, err := coord.sessionDrainPolicy(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, allowed, "Stop's suspension must still gate an ordinary (uncapped-category) async wake")
	require.False(t, counted)
}

// TestSessionDrainPolicy_BgShellWake_CapsAtFive pins the capped category's
// unchanged behavior: exactly maxConsecutiveAutoResumes successful bg-shell
// auto-resumes are allowed, the next is refused.
func TestSessionDrainPolicy_BgShellWake_CapsAtFive(t *testing.T) {
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.WithValue(context.Background(), autoTurnCapAppliesCtxKey{}, true)
	sess, err := env.sessions.Create(context.Background(), "bg-shell-cap")
	require.NoError(t, err)

	for i := range maxConsecutiveAutoResumes {
		allowed, counted, err := coord.sessionDrainPolicy(ctx, sess.ID)
		require.NoError(t, err)
		require.True(t, allowed, "attempt %d must be allowed", i)
		require.True(t, counted)
		coord.bumpConsecutiveResume(sess.ID)
	}
	allowed, _, err := coord.sessionDrainPolicy(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, allowed, "the (maxConsecutiveAutoResumes+1)-th bg-shell auto-resume must be refused")
}

// TestWakeSession_BgShellNoTurnDrain_DoesNotBumpCap pins the
// onDrainTurnStarting mechanism directly for the ONE category it can still
// be observed in (async-job/delegation wakes are unconditionally uncounted
// now, so this only matters for capped bg-shell-flavored wakes): a Drain
// that takes the no-turn branch (no debt at all) must never bump the cap
// counter, even though wakeSession's own agent.Run call returns nil, nil
// (from wakeSession's perspective, success).
//
// Revert-check performed: reverted the `counted && admission.didReachProvider()`
// guard to the old unconditional `if counted { ... bumpConsecutiveResume
// ... }` -- this test's `require.Zero(t, coord.consecutiveResume(...))`
// FAILED (the counter was 1 after a no-turn Drain). Restored the guard;
// re-ran, passed.
func TestWakeSession_BgShellNoTurnDrain_DoesNotBumpCap(t *testing.T) {
	env := testEnv(t)
	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	ledger := newWorkLedger(coord.notifyAsyncCompletion)
	ledger.store = store
	ledger.coord = coord
	coord.asyncJobs = ledger

	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: newNeverCalledModel(t), FastModel: newNeverCalledModel(t), SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
	})
	agent := sa.(*sessionAgent)
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "bg-shell-no-turn")
	require.NoError(t, err)
	coord.subAgentDrivers.register(sess.ID, subAgentDriver{agent: agent, call: SessionAgentCall{SessionID: sess.ID}})
	// No debt seeded at all -- decideDrainTurn takes the no-turn branch.

	bgShellCtx := context.WithValue(context.Background(), autoTurnCapAppliesCtxKey{}, true)
	err = coord.wakeSession(bgShellCtx, jobIdentity{owner: sess.ID, toolCallID: "shell-1"}, true)
	require.NoError(t, err)
	require.Zero(t, coord.consecutiveResume(sess.ID), "a no-turn Drain must never bump the auto-turn cap counter")
}
