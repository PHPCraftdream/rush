package app

// Cancellation requested externally while the optional todo reminder is running
// must remain the run outcome, rather than being treated as a failed reminder
// and retaining the prior turn. The drop branch must call stopError() to detect
// the pending cancellation.

import (
	"context"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// REVERT CHECK: removing the stopError() call from nudgePhase's drop branch
// swallows the external cancel_requested flag and keeps the executor's first
// answer with exit_reason "end_turn" instead of "canceled".
func TestRunLoop_TodoNudgeCancellationDuringTurnExitsCanceled(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		switch n {
		case 1:
			// First turn succeeds; executor answer will be kept.
			loopText(w, "a", "first answer", 11, 3)
		case 2:
			// Reminder turn: request cancel BEFORE the provider responds,
			// then respond with a provider error (400 Bad Request).
			// This keeps the run context alive (l.ctx.Err() == nil) but
			// sets cancel_requested in the session.
			require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID))

			// Make the reminder provider call fail for its own reason.
			// A 400 Bad Request is a terminal error (no retry).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad request: rejected","type":"x"}}`))
		default:
			h.t.Errorf("unexpected provider request #%d", n)
			loopText(w, "x", "unexpected", 1, 1)
		}
	})
	require.NoError(t, h.app.Sessions.SetTodos(context.Background(), h.sessionID, []session.Todo{
		{Content: "deploy after the answer", Status: session.TodoStatusPending},
	}, nil))

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	// The run should exit with reason "canceled" because:
	// 1. The reminder failed for its own reason (provider error)
	// 2. The drop branch enters (err != nil, l.ctx.Err() == nil)
	// 3. The drop branch calls stopError() (or should)
	// 4. stopError() detects cancel_requested and returns runIncompleteError("canceled")
	var inc *runIncompleteError
	require.ErrorAs(t, err, &inc)
	require.Equal(t, "canceled", inc.reason)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	canceled, cancelErr := h.app.Sessions.IsCancelRequested(context.Background(), h.sessionID)
	require.NoError(t, cancelErr)
	require.False(t, canceled, "honoured cancellation must be cleared")
	require.EqualValues(t, 2, h.requests.Load(), "first turn and in-flight reminder only")
	require.Equal(t, "first answer", res.FinalText, "the executor response remains the answer")
}
