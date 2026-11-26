package app

// A text-only PASS on a cancelled run must end unverified, not pass.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRunLoop_ReviewerCompletedThenParentCanceledKeepsExecutorAnswer(t *testing.T) {
	ctx, cancel := context.WithCancel(loopCtx(t))
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, body []byte, _ bool, _ int) {
		_, lastUser, _ := lastTurnParts(body)
		if strings.Contains(lastUser, "independent reviewer") {
			loopText(w, "review", "VERDICT: PASS", 13, 4)
			return
		}
		loopText(w, "primary", strings.Repeat("executor answer ", 12000), 11, 3)
	})
	h.app.config.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	oldSeam := cliLoopTurnDoneSeam
	var turnDone int
	cliLoopTurnDoneSeam = func() {
		turnDone++
		if turnDone == 2 {
			cancel()
		}
	}
	t.Cleanup(func() {
		cliLoopTurnDoneSeam = oldSeam
		cancel()
	})

	res, _, err := h.run(ctx, RunOverrides{ModelRole: config.SelectedModelTypeSmart})

	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.Equal(t, strings.Repeat("executor answer ", 12000), res.FinalText)
	require.Equal(t, "unverified", res.ReviewVerdict)
	require.Contains(t, res.Review, "VERDICT: PASS")
	warnings := strings.Join(res.Warnings, " | ")
	require.Contains(t, warnings, "reviewer package truncated")
	require.Contains(t, warnings, "reviewer passed the run without any read-tool check")
}

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

	// A failed reviewer response still returns a cancellation error, while the
	// primary answer remains intact and the cancel request is consumed.
	var inc *runIncompleteError
	require.ErrorAs(t, err, &inc)
	require.Equal(t, "canceled", inc.Reason)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	canceled, cancelErr := h.app.Sessions.IsCancelRequested(context.Background(), h.sessionID)
	require.NoError(t, cancelErr)
	require.False(t, canceled, "honoured cancellation must be cleared")
	require.EqualValues(t, 2, h.requests.Load(), "first turn and in-flight review turn only")
	require.Equal(t, "first answer", res.FinalText, "the executor response remains the answer")
}
