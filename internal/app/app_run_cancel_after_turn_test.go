package app

// A cancellation (Ctrl-C, --timeout) that lands after a loop turn finished
// cleanly but before the loop's next decision must end the run as a
// cancellation: the envelope says "canceled", so the returned error must be
// non-nil too (cmd/run.go exits non-zero on it). It used to return the last
// turn's own error -- nil -- so `rush run` exited 0 with a canceled envelope.

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestRunNonInteractive_CancelAfterSuccessfulTurnReturnsCancellation forces the
// interleaving with cliLoopTurnDoneSeam: the cancel fires exactly when the
// first turn has returned successfully.
//
// Revert-check performed: returned the bare `runErr` from the ctx.Err() branch
// again (dropping the `runErr == nil && ctx.Err() != nil` promotion) -- err was
// nil while res.ExitReason was "canceled", so require.ErrorIs FAILED.
func TestRunNonInteractive_CancelAfterSuccessfulTurnReturnsCancellation(t *testing.T) {
	app, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		admissionWriteSSE(w, []string{admissionSSEText("t", "done"), admissionSSEStop("t", "stop")})
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cliLoopTurnDoneSeam = cancel
	t.Cleanup(func() { cliLoopTurnDoneSeam = nil })

	res, err := app.RunNonInteractiveWithResult(ctx, io.Discard, "hello", RunOverrides{Origin: message.OriginCLI},
		true, RunModeJSON, sessionID, false)

	require.ErrorIs(t, err, context.Canceled, "a cancellation after a clean turn must not exit 0")
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.Contains(t, res.Error, "context canceled")
}
