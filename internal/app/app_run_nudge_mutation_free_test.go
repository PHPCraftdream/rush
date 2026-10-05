package app

// C9-8: the unfinished-todos reminder turn reached ExecuteRun with loopTurn
// only, so mutationFree() was false and the reminder's admission re-ran the
// invocation's own session setup: ClearCancelRequest ate an operator's
// `sessions cancel` that landed in the admission window, and the re-setup
// rewrote system prompt, reasoning effort and the model slots over a mid-run
// operator change (R3C-6). nudgeTurn makes the reminder a mutation-free
// follow-up, like a Drain.

import (
	"context"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// REVERT CHECK: this loop test alone cannot fail without the fix -- a cancel
// planted this early is honoured by nudgePhase's own stopError() BEFORE any
// ExecuteRun, in the fixed and the unfixed tree alike; it pins the visible
// contract. The revert-sensitive assertions are
// TestRunLoop_MidRunModelSlotChangeSurvivesTheReminderTurn and
// TestRunRequest_NudgeTurnIsMutationFree below.
func TestRunLoop_CancelBeforeTheReminderTurnExitsCanceled(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		if n == 1 {
			loopText(w, "a", "first answer", 11, 3)
			return
		}
		h.t.Errorf("unexpected provider request #%d: no reminder may run once a cancel is pending", n)
		loopText(w, "x", "unexpected", 1, 1)
	})
	require.NoError(t, h.app.Sessions.SetTodos(context.Background(), h.sessionID, []session.Todo{
		{Content: "deploy after the answer", Status: session.TodoStatusPending},
	}, nil))
	h.afterFirstTurn(func() {
		// What `sessions cancel` does: the same service call sets the flag.
		require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID))
	})

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	var inc *runIncompleteError
	require.ErrorAs(t, err, &inc)
	require.Equal(t, "canceled", inc.reason)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.EqualValues(t, 1, h.requests.Load(), "the first turn only: the reminder never reaches the provider")
}

// REVERT CHECK: dropping nudgeTurn from mutationFree() makes the reminder
// turn's admission re-persist the invocation's model slots; the operator's
// mid-run change is overwritten and the read-back returns "probe" (want
// "probe2"). This test FAILED on the SmartModelID assertion.
func TestRunLoop_MidRunModelSlotChangeSurvivesTheReminderTurn(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		switch n {
		case 1:
			loopText(w, "a", "first answer", 11, 3)
		case 2, 3: // the two budgeted reminder turns
			loopText(w, "n", "still working on it", 8, 2)
		default:
			h.t.Errorf("unexpected provider request #%d", n)
			loopText(w, "x", "unexpected", 1, 1)
		}
	})
	require.NoError(t, h.app.Sessions.SetTodos(context.Background(), h.sessionID, []session.Todo{
		{Content: "deploy after the answer", Status: session.TodoStatusPending},
	}, nil))
	h.afterFirstTurn(func() {
		// The operator's mid-run model change (R3C-6).
		require.NoError(t, h.app.Sessions.UpdateModels(context.Background(), h.sessionID,
			&session.ModelSlotUpdate{Provider: "openaicompat", Model: "probe2"}, nil))
	})

	res, _, err := h.run(loopCtx(t), RunOverrides{SmartModel: "openaicompat/probe"})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.EqualValues(t, 3, h.requests.Load(), "the first turn and the two budgeted reminders")
	sess, gErr := h.app.Sessions.Get(context.Background(), h.sessionID)
	require.NoError(t, gErr)
	require.Equal(t, "probe2", sess.SmartModelID,
		"the reminder turn must not re-persist the invocation's slot over the operator's change")
	require.Equal(t, "openaicompat", sess.SmartModelProvider)
}

// REVERT CHECK: without nudgeTurn in mutationFree() this returns false and
// the test fails.
func TestRunRequest_NudgeTurnIsMutationFree(t *testing.T) {
	require.True(t, RunRequest{nudgeTurn: true}.mutationFree())
	require.False(t, RunRequest{}.mutationFree())
}
