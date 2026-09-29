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
