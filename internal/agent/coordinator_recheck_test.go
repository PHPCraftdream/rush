// The 60s recheck pass and its ticker lifecycle (docs/plans/2026-09-28-
// async-phase4-durable-core.md sec.3.4 rule (b)/sec.3.5, step 4 review).
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

// TestRecheckSet_SessionLockBusyGoesIntoRecheckSet_RetriedByPass pins doc
// sec.3.4 rule (b) and sec.6's "orphaned Drain"/"sessions locks holding the
// lock at check time" scenarios: a cross-process session-lock-busy refusal
// (session.SessionLockBusyError -- exactly what `sessions locks/kill/reap`
// holding the lock produces) is never a settle-by-failure event; the session
// goes into the recheck set instead, and the next RecheckPass retries it --
// once the lock clears, the wake is delivered, not lost.
//
// W-DRAIN B3 fix (docs/reviews/2026-09-29-async-phase4-round1.md, "Tests
// (B-b, C-b)": "fake bypasses runOwned/abandon -- cannot see B2"): this used
// to wrap the driver's Run to manually return SessionLockBusyError, which
// never touched runOwned's own lock acquisition or onSessionIdle's release
// funnel at all -- a B2 regression (the release-triggered hot loop) could
// never have failed this test. Rewritten to hold a REAL OS session lock (a
// second, independent acquisition on the same lock file -- exactly what
// `sessions locks/kill/reap` or another live rush process looks like) and
// drive the call through sessionAgentNoProbeWrapper (coordinator_wake_lock_
// refusal_test.go), which defeats wakeSession's OWN pre-submit shared-probe
// optimization so the call actually reaches runOwned's real
// TryAcquireSessionLockWithOptions and its real onSessionIdle release path.
//
// REVERT CHECK 1 (recheck-set half, pre-existing): removed
// `c.addToRecheckSet(job.owner)` from recordDrainOutcome's turnAttemptRefused
// branch (coordinator_drain_policy.go) -- this test's `require.True(t,
// inSet, ...)` FAILED (the set stayed empty) and the subsequent RecheckPass
// found nothing to retry (requests stayed 0 forever). Restored the call;
// re-ran, passed.
//
// REVERT CHECK 2 (no-hot-loop half, re-verified live for this rewrite):
// disabled onSessionIdleHook's consumeAdmissionRefusedRelease gate
// (supervision.go) -- `require.Less(t, int(attempts.Load()), maxAttempts)`
// FAILED (attempts hit maxAttempts within a fraction of a second, the exact
// wakeSession -> runOwned lock refusal -> onSessionIdleHook ->
// recheckDebtOnRelease -> wakeSession chain B2 describes). Restored the
// gate; re-ran, attempts stayed at exactly 1.
func TestRecheckSet_SessionLockBusyGoesIntoRecheckSet_RetriedByPass(t *testing.T) {
	env := testEnv(t)
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "recheck-set-lock-busy")
	require.NoError(t, err)

	// A second, real acquisition of the SAME session's OS lock -- another
	// holder, exactly like a foreign rush process or `sessions locks/kill`.
	foreignLock, err := session.TryAcquireSessionLock(env.workingDir, sess.ID)
	require.NoError(t, err)
	var lockReleasedOnce atomic.Bool
	releaseForeignLock := func() {
		if lockReleasedOnce.CompareAndSwap(false, true) {
			_ = foreignLock.Release()
		}
	}
	t.Cleanup(releaseForeignLock)

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
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
		// Wired for real, matching production: a release-triggered
		// recheckDebtOnRelease chain (if the B2 fix ever regressed) fires
		// through this exact hook.
		OnSessionIdle: coord.onSessionIdleHook,
	})
	agent := sa.(*sessionAgent)
	// The wrapper defeats wakeSession's `agent.(*sessionAgent)` pre-submit
	// shared-probe type-assertion, forcing the call through runOwned's own
	// (real) lock acquisition instead of being intercepted earlier.
	coord.subAgentDrivers.register(sess.ID, subAgentDriver{agent: sessionAgentNoProbeWrapper{agent}, call: SessionAgentCall{SessionID: sess.ID}})

	// Seed real VISIBLE debt (a real pull).
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

	// Bounded safety valve, exactly like TestOnSessionIdleHook_
	// AdmissionRefusedRelease_NoImmediateRelaunch: if a regression makes the
	// release chain self-perpetuate, this converges the test instead of
	// hanging it, while still failing the `require.Less` assertion below.
	const maxAttempts = 6
	var attempts atomic.Int32
	wakeSessionAttemptSeam = func() {
		n := attempts.Add(1)
		if n >= maxAttempts {
			releaseForeignLock()
		}
	}
	t.Cleanup(func() { wakeSessionAttemptSeam = nil })

	err = coord.wakeSession(ctx, jobIdentity{owner: sess.ID, toolCallID: "call-1"}, true)
	require.Error(t, err, "the real lock-busy refusal must surface from this direct call")
	require.Zero(t, requests.Load(), "the refused attempt must never reach the provider")

	// Let any release-triggered chain fully settle before asserting it never
	// ran away.
	require.Eventually(t, func() bool { return attempts.Load() >= 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	require.Less(t, int(attempts.Load()), maxAttempts,
		"a session-lock-busy refusal must not synchronously spin into repeated Drain attempts while the foreign lock is still held")

	coord.recheckMu.Lock()
	_, inSet := coord.recheckSet[sess.ID]
	coord.recheckMu.Unlock()
	require.True(t, inSet, "a session-lock-busy refusal must land in the recheck set, not be forgotten")

	debtStillOpen, err := store.ReactionDebtExists(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, debtStillOpen, "an admission refusal must never close the debt")

	// Now the lock genuinely clears (this test's own release, not the safety
	// valve) and the next RecheckPass retries the session for real.
	releaseForeignLock()
	coord.RecheckPass(ctx)
	require.Eventually(t, func() bool { return requests.Load() > 0 }, 2*time.Second, 10*time.Millisecond,
		"the 60s pass must retry a recheck-set session and eventually deliver the wake once the real lock is released")
}

// TestRecheckTicker_StopsCleanly_NoGoroutineLeak confirms StartRecheckTicker's
// goroutine actually exits once stopped (via CancelAll, production's own
// shutdown path) rather than leaking -- doc sec.3.5's 60s pass must not
// outlive the coordinator.
//
// REVERT CHECK: commented out the `c.StopRecheckTicker()` call inside
// CancelAll (coordinator_interrupt.go) -- this test's `<-done` select
// FAILED (timed out after 2s: the ticker goroutine was still alive, parked
// on its own ctx that CancelAll no longer cancelled). Restored the call
// (byte-identical diff confirmed); re-ran, passed.
func TestRecheckTicker_StopsCleanly_NoGoroutineLeak(t *testing.T) {
	coord := &coordinator{currentAgent: &mockSessionAgent{}}

	coord.StartRecheckTicker()
	coord.StartRecheckTicker() // idempotent: must not spawn a second goroutine
	require.NotNil(t, coord.recheckStop)
	done := coord.recheckDone
	require.NotNil(t, done)

	select {
	case <-done:
		t.Fatal("the ticker goroutine must not have exited before it was ever stopped")
	default:
	}

	coord.CancelAll() // production's shutdown path
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the ticker goroutine must exit once CancelAll stops it -- no leak")
	}

	// A second CancelAll (idempotent shutdown) must not panic or hang.
	coord.CancelAll()
}
