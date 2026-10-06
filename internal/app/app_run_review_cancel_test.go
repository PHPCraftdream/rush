package app

// Same defect class as the already-fixed #1227 (the nudge drop branch): a
// reviewer turn that failed for a reason of its own kept the executor's
// answer and exited clean WITHOUT checking the operator's stop signals, so a
// `sessions cancel` landing during the review turn was swallowed — the run
// ended end_turn and left the honoured cancel flag pending.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// REVERT CHECK: removing the stopError() call from closePhase's
// reviewFailureKeepsPrimary branch swallows the cancel_requested flag set
// during the reviewer turn: the run exits clean (err == nil) with the
// executor's answer instead of ending canceled, and the flag stays pending.
func TestRunLoop_ReviewerTurnCancellationDuringTurnExitsCanceled(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, _ bool, _ int) {
		_, lastUser, _ := lastTurnParts(body)
		if strings.Contains(lastUser, "independent reviewer") {
			// The review turn: request the cancel BEFORE the provider
			// responds, then fail the turn for its own reason (a 400 is
			// terminal, no retry). The run's ctx stays alive.
			require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad request: rejected","type":"x"}}`))
			return
		}
		// First turn: the executor answers cleanly.
		loopText(w, "a", "first answer", 11, 3)
	})
	h.app.config.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: "probe"})

	res, _, err := h.run(loopCtx(t), RunOverrides{ModelRole: config.SelectedModelTypeSmart})

	// The review failed for its own reason, but the operator's cancel was
	// requested while it ran: the run must end canceled, never as a
	// success that keeps the primary answer.
	var inc *runIncompleteError
	require.ErrorAs(t, err, &inc)
	require.Equal(t, "canceled", inc.reason)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	canceled, cancelErr := h.app.Sessions.IsCancelRequested(context.Background(), h.sessionID)
	require.NoError(t, cancelErr)
	require.False(t, canceled, "honoured cancellation must be cleared")
	require.EqualValues(t, 2, h.requests.Load(), "first turn and in-flight review turn only")
	require.Equal(t, "first answer", res.FinalText, "the executor response remains the answer")
}
