// Round-7 fixes of the `rush run` loop (docs/reviews/2026-09-30-async-phase4-
// round7.md, W7-SHELL: R7C-1): same harness as app_run_loop_test.go.
package app

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// requestCancelOnWrite marks the session cancel-requested the moment the loop
// prints substr: an operator's `sessions cancel` that lands while it waits.
type requestCancelOnWrite struct {
	syncBuffer
	substr string
	cancel func()
}

func (w *requestCancelOnWrite) Write(p []byte) (int, error) {
	n, err := w.syncBuffer.Write(p)
	if w.cancel != nil && bytes.Contains(p, []byte(w.substr)) {
		w.cancel()
	}
	return n, err
}

// r7RunCtx bounds a test that only reaches its assertions when the fix works:
// without it the loop waits for the (never finishing) job until this deadline.
func r7RunCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// R7C-1: `sessions cancel` reaches a loop that waits between turns on running
// work. Before the fix the flag was read only before a paid turn, so the loop
// waited for the held job (here: forever) and the run ended at its ctx
// deadline instead of "canceled".
//
// Revert-check: without the cancel check in nextStep's WorkOpen branch both
// cases run into the ctx deadline.
func TestRunLoop_CancelWhileWaitingOnRunningWorkEndsCanceled(t *testing.T) {
	cases := map[string]func(h *loopHarness, stderr *requestCancelOnWrite){
		"requested before the wait": func(h *loopHarness, _ *requestCancelOnWrite) {
			h.afterFirstTurn(func() { require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID)) })
		},
		"requested while waiting": func(h *loopHarness, stderr *requestCancelOnWrite) {
			stderr.cancel = func() { require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID)) }
		},
	}
	for name, arm := range cases {
		t.Run(name, func(t *testing.T) {
			h := r6HeldFirstJobHarness(t)
			writes := logEndedReasonWrites(t, h.app)
			stderr := &requestCancelOnWrite{substr: "still has open work"}
			arm(h, stderr)
			cliLoopStderr = stderr
			t.Cleanup(func() { cliLoopStderr = nil })

			res, out, err := h.run(r7RunCtx(t), RunOverrides{})

			require.Error(t, err)
			var inc *runIncompleteError
			require.ErrorAs(t, err, &inc)
			require.Equal(t, "canceled", inc.reason)
			require.NotNil(t, res)
			require.Equal(t, "canceled", res.ExitReason)
			require.Contains(t, res.Error, "cancelled by user")
			require.Contains(t, out, `"exit_reason":"canceled"`)
			require.EqualValues(t, 2, h.requests.Load(), "the first turn's two steps (job call, then its result); no paid turn follows the cancel")
			require.Equal(t, "canceled", endedReasonOf(t, h.app, h.sessionID))
			require.Equal(t, []string{"", "end_turn", "canceled"}, writes())
		})
	}
}

// R7C-1: the Paced branch (waiting for the launch gate, up to two minutes)
// re-reads the cancel flag too.
//
// Revert-check: without the cancel check in nextStep's Paced branch the loop
// waits out the gate (an hour here) and the run ends at the ctx deadline.
func TestRunLoop_CancelWhilePacedEndsCanceled(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "R7-FIRST", 11, 3)
	})
	src := &scopeOverrideSource{
		ReactionDebtSource: driverSource(t, h.app),
		state:              agent.CLIScopeState{Drain: agent.DrainPaced, RetryAt: time.Now().Add(time.Hour)},
	}
	h.afterFirstTurn(func() {
		require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID))
		src.armed.Store(true)
	})
	var out, stderr syncBuffer
	overrides := RunOverrides{Origin: message.OriginCLI}
	l := &cliLoop{
		app: h.app, source: src, ctx: r7RunCtx(t), output: &out, mode: RunModeJSON, hideSpinner: true,
		overrides: overrides, turnOverrides: overrides, prompt: "do it",
		continueSessionID: h.sessionID, started: time.Now(), sessionID: h.sessionID,
		lastBuffered: &bytes.Buffer{}, stderr: &stderr,
	}

	res, err := l.run()

	var inc *runIncompleteError
	require.ErrorAs(t, err, &inc)
	require.Equal(t, "canceled", inc.reason)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.EqualValues(t, 1, h.requests.Load())
}
