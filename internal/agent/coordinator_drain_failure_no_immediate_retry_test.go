// B6/C5a (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): a failed
// Drain's OWN release must not trigger an immediate relaunch either --
// before this fix, rule (a)'s marker covered only no-turn Drains, so a
// transient provider failure's own release started recheckDebtOnRelease
// which re-submitted at once, capable of exhausting K=3 within seconds of
// one 503 (violating doc sec.3.4 rule (c) and sec.6's "a one-minute outage
// does not close the debt").
package agent

import (
	"context"
	"fmt"
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

// transientErrorResponse serves an HTTP 503, which fantasy's openaicompat
// client surfaces as a *fantasy.ProviderError classified classTransient.
func transientErrorResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprint(w, `{"error":{"message":"service unavailable","type":"server_error"}}`)
}

// TestFailedDrain_OwnReleaseDoesNotImmediatelyRelaunch is the end-to-end
// proof: a Drain that reaches a REAL provider and fails transiently must not
// have its own onSessionIdle release spin into another attempt. The debt
// stays open (not settled -- K=1 of 3), and the session lands in the 60s
// recheck set instead.
//
// Revert-check performed: removed the `call.IsDrain && err != nil &&
// !turnAttemptRefused(err)` marking block from agent_run.go's runOwned loop
// -- this test's `require.Equal(t, int32(1), attempts)` FAILED: attempts
// climbed past 1 (the goroutine chain wakeSession -> agent.Run (fails) ->
// onSessionIdleHook -> go recheckDebtOnRelease -> wakeSession -> ... ran
// repeatedly against the SAME probe server within the observation window,
// each hit incrementing wake_attempts, risking K=3 exhaustion in far under a
// minute). Restored the block; re-ran, attempts stayed at exactly 1.
func TestFailedDrain_OwnReleaseDoesNotImmediatelyRelaunch(t *testing.T) {
	env := testEnv(t)
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		transientErrorResponse(w)
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
	sess, err := env.sessions.Create(ctx, "failed-drain-no-retry")
	require.NoError(t, err)
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

	var attempts int32
	wakeSessionAttemptSeam = func() { atomic.AddInt32(&attempts, 1) }
	t.Cleanup(func() { wakeSessionAttemptSeam = nil })

	_ = coord.wakeSession(ctx, jobIdentity{owner: sess.ID, toolCallID: "call-1"}, true)

	// Let any (incorrectly) spawned relaunch goroutine reach the probe
	// server before asserting it never did. requests > 1 is expected and
	// irrelevant here -- fantasy's own HTTP client retries a single request
	// internally on a 503; what this test cares about is how many separate
	// wakeSession/Drain SUBMISSIONS happened (attempts), not HTTP retries
	// inside one submission.
	time.Sleep(300 * time.Millisecond)
	require.EqualValues(t, 1, atomic.LoadInt32(&attempts),
		"a failed Drain's own release must not trigger an immediate relaunch")
	require.Positive(t, atomic.LoadInt32(&requests), "the provider must have been reached at least once")

	debt, err := store.ReactionDebtExists(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, debt, "one transient failure (K=1 of 3) must not close the debt")

	notices, err := store.ListSessionNotices(ctx, sess.ID)
	require.NoError(t, err)
	for _, n := range notices {
		require.NotEqual(t, session.NoticeKindWakeFailed, n.Kind, "one transient failure must not write a wake-failed marker")
	}

	coord.recheckMu.Lock()
	_, inSet := coord.recheckSet[sess.ID]
	coord.recheckMu.Unlock()
	require.True(t, inSet, "a failed Drain's session must land in the 60s recheck set, never be forgotten")
}

// TestSettleByFailure_KThreeViaRealReleaseHookAndRecheckPass closes W-DRAIN
// item 9's "TestSettleByFailure_* mocks never fire onSessionIdle" gap for
// the SETTLEMENT case specifically: unlike coordinator_settle_by_failure_
// test.go's fixture (a bare mock SessionAgent, no OnSessionIdle wired at
// all), this drives K=3 through the REAL production chain end to end -- a
// real HTTP provider returning 503, the real onSessionIdleHook release path
// for the FIRST failure (rule (a): a failed Drain's own release does not
// immediately relaunch, proven above), and the real RecheckPass/wakeSession
// path for the two retries that follow, exactly how a genuine 60s pass
// would deliver them.
//
// Revert-check performed: changed drainFailureSettleThreshold from 3 to 4
// -- this test's post-3rd-pass assertions FAILED (debt still open, zero
// markers, requests stuck at 3). Restored 3; re-ran, passed.
func TestSettleByFailure_KThreeViaRealReleaseHookAndRecheckPass(t *testing.T) {
	env := testEnv(t)
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		transientErrorResponse(w)
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
	sess, err := env.sessions.Create(ctx, "k-three-real-release")
	require.NoError(t, err)
	coord.subAgentDrivers.register(sess.ID, subAgentDriver{agent: agent, call: SessionAgentCall{SessionID: sess.ID}})

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

	// Attempt 1 of 3: the real release hook's own onSessionIdleHook fires
	// (via wakeSession's own Run/runOwned/release chain), landing the
	// session in the recheck set per rule (a) without an immediate relaunch.
	err = coord.wakeSession(ctx, jobIdentity{owner: sess.ID, toolCallID: "call-1"}, true)
	require.Error(t, err)
	debt, err := store.ReactionDebtExists(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, debt, "attempt 1: debt must survive under K=3")

	// Attempts 2 and 3: the 60s pass's own real path (RecheckPass ->
	// wakeSession), exactly as a genuine background tick would deliver them.
	coord.RecheckPass(ctx)
	debt, err = store.ReactionDebtExists(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, debt, "attempt 2: debt must still survive under K=3")

	coord.RecheckPass(ctx)
	debt, err = store.ReactionDebtExists(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, debt, "attempt 3 (K=3) via the real RecheckPass path must close the debt")

	notices, err := store.ListSessionNotices(ctx, sess.ID)
	require.NoError(t, err)
	markers := 0
	for _, n := range notices {
		if n.Kind == session.NoticeKindWakeFailed {
			markers++
		}
	}
	require.Equal(t, 1, markers, "exactly one marker at K=3 via the real release/recheck-pass path")
	require.GreaterOrEqual(t, atomic.LoadInt32(&requests), int32(3), "the real provider must have been reached at least 3 times")
}
