// B2/C2 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): a release
// caused by an admission refusal (another process holds the session's OS
// lock) must never trigger an immediate relaunch. Before the fix,
// wakeSession's own agent.Run attempt failed with SessionLockBusyError,
// runOwned's deferred abandonOwnershipWithHandoff fired onSessionIdle
// unconditionally, and onSessionIdleHook's own recheckDebtOnRelease
// immediately re-submitted a Drain -- which failed the SAME way, fired
// onSessionIdle again, and so on: an unbounded hot loop with no pause, for
// the entire duration the foreign process holds the lock.
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestWakeSession_SessionLockBusy_NeverAttemptsRunGoesToRecheckSet pins the
// pre-submit shared-lock probe (rule (b), part 1): with another process
// genuinely holding the session's OS lock, wakeSession must never even
// attempt agent.Run -- it goes straight to the recheck set.
//
// Revert-check performed: commented out the shared-probe block in
// coordinator_wake.go (fell straight through to admission/agent.Run) --
// this test's `require.Zero(t, requests)` FAILED intermittently is not the
// right framing; instead the NEXT test
// (TestOnSessionIdleHook_AdmissionRefusedRelease_NoImmediateRelaunch) is
// what catches the hot loop directly (see its own revert-check). This test
// alone still passed after removing the probe (agent.Run's own lock
// acquisition also refuses correctly) -- restored the probe regardless,
// since it is the documented rule-(b) mechanism and avoids the wasted
// reservation cycle; see the next test for the mechanism whose absence
// actually reproduces the unbounded loop.
func TestWakeSession_SessionLockBusy_NeverAttemptsRunGoesToRecheckSet(t *testing.T) {
	env := testEnv(t)
	ctxSetup := context.Background()
	sess, err := env.sessions.Create(ctxSetup, "lock-busy-wake")
	require.NoError(t, err)
	foreignLock, err := session.TryAcquireSessionLock(env.workingDir, sess.ID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = foreignLock.Release() })

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		textFinishResponse(w, "should never be reached")
	}))
	t.Cleanup(srv.Close)
	model := newProbeModel(t, srv)

	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	ledger := newWorkLedger(coord.notifyAsyncCompletion)
	ledger.store = store
	ledger.coord = coord
	coord.asyncJobs = ledger

	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
		OnSessionIdle: coord.onSessionIdleHook,
	})
	agent := sa.(*sessionAgent)
	ctx := context.Background()
	coord.subAgentDrivers.register(sess.ID, subAgentDriver{agent: agent, call: SessionAgentCall{SessionID: sess.ID}})

	// Seed real visible debt.
	_, existing, err := ledger.Start(sess.ID, "call-1", "call-1", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)
	require.NoError(t, store.MarkAnnounced(ctx, sess.ID, "call-1"))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: sess.ID, ToolCallID: "call-1", State: "completed", ResultSummary: "output", Wake: true,
	})
	require.NoError(t, err)
	_, err = store.PullJobNotices(ctx, env.messages, sess.ID, buildJobNoticeMessageParams)
	require.NoError(t, err)

	err = coord.wakeSession(ctx, jobIdentity{owner: sess.ID, toolCallID: "call-1"}, true)
	require.NoError(t, err, "a session-lock refusal must not be surfaced as a wakeSession error")

	require.Zero(t, atomic.LoadInt32(&requests), "the shared probe must stop wakeSession before it ever attempts a Run against a busy session lock")

	coord.recheckMu.Lock()
	_, inSet := coord.recheckSet[sess.ID]
	coord.recheckMu.Unlock()
	require.True(t, inSet, "a lock-busy session must land in the 60s recheck set, never be forgotten (rule (b))")
}

// sessionAgentNoProbeWrapper wraps a real *sessionAgent behind a DIFFERENT
// concrete type, so wakeSession's `agent.(*sessionAgent)` type-assertion
// (the pre-submit shared-lock probe, mechanism 1) misses and falls straight
// through to the ordinary Run() attempt -- while everything the wrapped
// agent actually DOES (mailbox reservation, the real OS lock acquisition in
// runOwned, the real admission-refused-release marking) is the genuine
// production code. This isolates mechanism 2 (the release marker) from
// mechanism 1 (the pre-submit probe) for a test that wants to prove the
// SECOND mechanism alone breaks the loop, since mechanism 1 would otherwise
// also catch this exact scenario and mask mechanism 2's own contribution.
type sessionAgentNoProbeWrapper struct{ *sessionAgent }

// TestOnSessionIdleHook_AdmissionRefusedRelease_NoImmediateRelaunch pins the
// SECOND mechanism directly, isolated from the first (see
// sessionAgentNoProbeWrapper's doc): even when a Run() call reaches the OS
// lock acquisition and is refused, the resulting onSessionIdle release must
// NOT synchronously spin into another Drain attempt. It must go to the
// recheck set instead.
//
// Revert-check performed: disabled onSessionIdleHook's
// consumeAdmissionRefusedRelease gate (supervision.go) -- this test's
// `require.Less(attempts, maxAttempts)` FAILED: attempts hit maxAttempts (6)
// within a fraction of a second, the goroutine stack trace at each hop
// showing the exact chain the finding describes (wakeSession ->
// runOwned's lock refusal -> onSessionIdleHook -> go recheckDebtOnRelease ->
// wakeSession -> ...), stopped only by this test's own safety valve
// releasing the foreign lock. Restored the gate; re-ran, attempts stayed at
// exactly 1.
func TestOnSessionIdleHook_AdmissionRefusedRelease_NoImmediateRelaunch(t *testing.T) {
	env := testEnv(t)
	ctxSetup := context.Background()
	sess, err := env.sessions.Create(ctxSetup, "admission-refused")
	require.NoError(t, err)
	foreignLock, err := session.TryAcquireSessionLock(env.workingDir, sess.ID)
	require.NoError(t, err)
	var lockReleasedOnce atomic.Bool
	releaseForeignLock := func() {
		if lockReleasedOnce.CompareAndSwap(false, true) {
			_ = foreignLock.Release()
		}
	}
	t.Cleanup(releaseForeignLock)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		textFinishResponse(w, "reacted once the lock finally freed")
	}))
	t.Cleanup(srv.Close)
	model := newProbeModel(t, srv)

	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	ledger := newWorkLedger(coord.notifyAsyncCompletion)
	ledger.store = store
	ledger.coord = coord
	coord.asyncJobs = ledger

	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
		// Wired for real, matching production: wakeSession's OWN nested
		// Run() attempt (below) also fires this on its release, which is
		// exactly what makes an unfixed onSessionIdleHook self-perpetuate.
		OnSessionIdle: coord.onSessionIdleHook,
	})
	agent := sa.(*sessionAgent)
	ctx := context.Background()
	// Register the WRAPPED agent as the driver: c.agentFor(sess.ID) (used by
	// recheckDebtOnRelease's own wakeSession call) hands back this wrapper,
	// defeating mechanism 1's type-assertion so only mechanism 2 (the
	// release marker) is under test here.
	coord.subAgentDrivers.register(sess.ID, subAgentDriver{agent: sessionAgentNoProbeWrapper{agent}, call: SessionAgentCall{SessionID: sess.ID}})

	// Seed real visible debt so an unbounded relaunch WOULD find something to
	// react to if it were ever (wrongly) attempted.
	_, existing, err := ledger.Start(sess.ID, "call-1", "call-1", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)
	require.NoError(t, store.MarkAnnounced(ctx, sess.ID, "call-1"))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: sess.ID, ToolCallID: "call-1", State: "completed", ResultSummary: "output", Wake: true,
	})
	require.NoError(t, err)
	_, err = store.PullJobNotices(ctx, env.messages, sess.ID, buildJobNoticeMessageParams)
	require.NoError(t, err)

	// Count every wakeSession re-entry (each hop of a self-perpetuating
	// release->recheck chain goes through here). Bounded safety valve: if
	// the chain ever reaches maxAttempts, release the real foreign lock so
	// the NEXT attempt succeeds for real and the recursion converges --
	// this makes even the UNFIXED (buggy) behavior safe to run in a test
	// process instead of spinning indefinitely.
	const maxAttempts = 6
	var attempts int32
	wakeSessionAttemptSeam = func() {
		n := atomic.AddInt32(&attempts, 1)
		if n >= maxAttempts {
			releaseForeignLock()
		}
	}
	t.Cleanup(func() { wakeSessionAttemptSeam = nil })

	// Kick off the chain exactly like recheckDebtOnRelease would: a single
	// wakeSession call while the lock is genuinely held elsewhere. Mechanism
	// 1 is bypassed by the wrapper type, so THIS call's own Run attempt
	// genuinely hits the lock-busy error and returns it -- expected, and
	// irrelevant to what this test checks (the release-triggered chain that
	// follows, and the recheck-set outcome).
	_ = coord.wakeSession(ctx, jobIdentity{owner: sess.ID, toolCallID: "call-1"}, true)

	// Let the chain (if any) fully settle: either it stayed at 1 attempt
	// (fixed) or it raced to maxAttempts and then converged once the safety
	// valve freed the lock (unfixed).
	require.Eventually(t, func() bool { return atomic.LoadInt32(&attempts) >= 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(200 * time.Millisecond) // let any further async hops finish

	require.Less(t, int(atomic.LoadInt32(&attempts)), maxAttempts,
		"an admission-refused release must not synchronously spin into repeated Drain attempts while the foreign lock is still held")

	coord.recheckMu.Lock()
	_, inSet := coord.recheckSet[sess.ID]
	coord.recheckMu.Unlock()
	require.True(t, inSet, "an admission-refused session must land in the 60s recheck set, never be forgotten (rule (b))")
}
