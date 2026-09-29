// B1 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): a release
// re-check's own debt-existence read must never bound the Drain turn it
// triggers. Before the fix, recheckDebtOnRelease (supervision.go) ran the
// WHOLE wakeSession/agent.Run/runOwned turn loop under the SAME context as
// its 30s debt-check deadline -- a Drain turn slower than that budget hit
// DeadlineExceeded, was classified as a terminal provider error, and settled
// the debt by failure (a visible wake_failed marker) even though the
// provider was still legitimately working.
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

// TestRecheckDebtOnRelease_DrainOutlivesCheckBudget_StillCompletes shrinks
// recheckDebtCheckBudget to a few milliseconds (a var precisely so a test
// can do this instead of waiting out a real 30s window) and makes the
// probe provider respond slower than that budget. The Drain triggered by
// recheckDebtOnRelease must still reach the provider and successfully react
// -- no DeadlineExceeded, no settled-by-failure marker.
//
// Revert-check performed: changed recheckDebtOnRelease's wakeSession call
// back to `c.wakeSession(checkCtx, id, true)` (the pre-fix shape) -- this
// test FAILED: the provider request never completed inside runOwned before
// checkCtx's tiny deadline fired, the turn errored with DeadlineExceeded,
// and a "wake_failed" session notice was persisted (settled by failure)
// instead of the real "reacted" reply. Restored `context.Background()`;
// re-ran, passed.
func TestRecheckDebtOnRelease_DrainOutlivesCheckBudget_StillCompletes(t *testing.T) {
	origBudget := recheckDebtCheckBudget
	recheckDebtCheckBudget = 15 * time.Millisecond
	t.Cleanup(func() { recheckDebtCheckBudget = origBudget })

	const providerDelay = 200 * time.Millisecond
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(providerDelay)
		atomic.AddInt32(&requests, 1)
		textFinishResponse(w, "reacted after the check budget expired")
	}))
	t.Cleanup(srv.Close)
	model := newProbeModel(t, srv)

	env := testEnv(t)
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
	})
	agent := sa.(*sessionAgent)
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "b1-recheck-deadline")
	require.NoError(t, err)
	coord.subAgentDrivers.register(sess.ID, subAgentDriver{agent: agent, call: SessionAgentCall{SessionID: sess.ID}})

	// Seed real VISIBLE debt: claim+finish a job, then pull its notice into
	// history -- exactly what decideDrainTurn requires to allow a turn.
	_, existing, err := ledger.Start(sess.ID, "call-1", "call-1", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)
	require.NoError(t, store.MarkAnnounced(ctx, sess.ID, "call-1"))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: sess.ID, ToolCallID: "call-1", State: "completed", ResultSummary: "output", Wake: true,
	})
	require.NoError(t, err)
	pulled, err := store.PullJobNotices(ctx, env.messages, sess.ID, buildJobNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)

	// The function under test: this must complete (well past providerDelay)
	// without the debt-check's own tiny budget ever reaching the turn.
	coord.recheckDebtOnRelease(sess.ID)

	require.EqualValues(t, 1, atomic.LoadInt32(&requests), "the Drain triggered by the release re-check must reach the provider")

	debtStillOpen, err := store.ReactionDebtExists(ctx, sess.ID)
	require.NoError(t, err)
	require.False(t, debtStillOpen, "a successful Drain turn must react to and clear the debt")

	notices, err := store.ListSessionNotices(ctx, sess.ID)
	require.NoError(t, err)
	for _, n := range notices {
		require.NotEqual(t, session.NoticeKindWakeFailed, n.Kind,
			"the Drain must not be misclassified as a terminal failure just because the CHECK budget (not the turn) was tiny")
	}
}

// TestRecheckDebtOnRelease_QueuedUserTurnSurvivesSlowDrain is W-DRAIN item C
// (docs/reviews/2026-09-29-async-phase4-round1.md, "Tests (B-b, C-b)"): a
// user turn dispatched behind a Drain that outlives recheckDebtCheckBudget
// must still run once the Drain releases the mailbox -- it must NOT be
// killed by the check's own tiny deadline. Before the B1 fix this test's
// scenario is exactly what the finding describes: "a queued user message in
// the same loop dies at 30s too", because the queued call ran inside the
// SAME ctx as the debt check (checkCtx), which this test's shrunk budget
// expires almost immediately.
//
// Revert-check performed: changed recheckDebtOnRelease's wakeSession call
// back to `c.wakeSession(checkCtx, id, true)` (passing the tiny-budget ctx
// through instead of context.Background()) -- this test's queued call
// FAILED: `agent.Run` for the queued prompt returned a context-deadline-
// shaped error instead of a real reply, because the queued turn inherited
// checkCtx and was torn down the instant recheckDebtOnRelease's own defer
// cancelled it. Restored `context.Background()`; re-ran, passed.
func TestRecheckDebtOnRelease_QueuedUserTurnSurvivesSlowDrain(t *testing.T) {
	origBudget := recheckDebtCheckBudget
	recheckDebtCheckBudget = 15 * time.Millisecond
	t.Cleanup(func() { recheckDebtCheckBudget = origBudget })

	const providerDelay = 300 * time.Millisecond
	drainReqStarted := make(chan struct{})
	var reqNum int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&reqNum, 1)
		if n == 1 {
			close(drainReqStarted)
			time.Sleep(providerDelay) // outlive the shrunk check budget
			textFinishResponse(w, "reacted after the check budget expired")
			return
		}
		textFinishResponse(w, "queued user turn's own reply")
	}))
	t.Cleanup(srv.Close)
	model := newProbeModel(t, srv)

	env := testEnv(t)
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
	})
	agent := sa.(*sessionAgent)
	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "b1-queued-turn-survives")
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
	pulled, err := store.PullJobNotices(ctx, env.messages, sess.ID, buildJobNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		coord.recheckDebtOnRelease(sess.ID)
	}()

	select {
	case <-drainReqStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the Drain never reached the provider")
	}

	// The Drain's own request is now in flight (past checkCtx's tiny
	// deadline by construction, since it sleeps providerDelay > 15ms) --
	// submit a REAL user turn for the SAME session now, while the mailbox is
	// busy: it must queue, not be refused or dropped. Run()'s own (nil, nil)
	// "queued" contract (agent_run.go: "internal/app detects the queued case
	// by (nil, nil)") means this call returns almost immediately once merely
	// ADMITTED to the queue -- it does NOT block until the queued turn
	// itself actually executes, so "not killed" has to be proven by the
	// queued turn's own SIDE EFFECT (a second real request reaching the
	// provider) below, not by this call's return value.
	queuedDone := make(chan struct{})
	var queuedErr error
	go func() {
		defer close(queuedDone)
		_, queuedErr = agent.Run(context.Background(), SessionAgentCall{
			SessionID: sess.ID, Prompt: "hello while the drain is busy", MaxOutputTokens: 1000,
		})
	}()

	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the Drain never returned")
	}
	select {
	case <-queuedDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() for the queued call never returned")
	}
	require.NoError(t, queuedErr, "a merely-queued admission is never an error")

	require.Eventually(t, func() bool { return atomic.LoadInt32(&reqNum) >= 2 }, 5*time.Second, 10*time.Millisecond,
		"the queued user turn must actually get its OWN turn once the Drain releases the mailbox -- it must not be killed/orphaned")
}
