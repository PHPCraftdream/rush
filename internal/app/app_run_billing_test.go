package app

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Revert-check: Pins agent/cli_active_work.go:22-24's billing latch recognition.
func TestCLIQuotaBilling1281WaitsForCommand(t *testing.T) {
	body, err := os.ReadFile("../agent/testdata/anthropic_credit_balance_1281.json")
	require.NoError(t, err)
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		require.Equal(t, 1, n, "no additional own request after billing failure")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	})
	ctx := loopCtx(t)
	_, err = h.app.asyncJobStore.Claim(ctx, session.ClaimParams{Owner: h.sessionID, ToolCallID: "billing-command", Kind: session.JobKindCommand, ToolName: "bash", Input: "durable test command"})
	require.NoError(t, err)
	require.NoError(t, h.app.asyncJobStore.MarkAnnounced(ctx, h.sessionID, "billing-command"))
	completed := make(chan error, 1)
	h.afterFirstTurn(func() {
		go func() {
			time.Sleep(100 * time.Millisecond)
			_, completionErr := h.app.asyncJobStore.Transition(ctx, session.TransitionParams{Owner: h.sessionID, ToolCallID: "billing-command", State: "completed", ResultSummary: "billing-command-output", Wake: true})
			completed <- completionErr
		}()
	})
	res, wire, runErr := h.run(ctx, RunOverrides{})
	require.NoError(t, <-completed)
	require.Error(t, runErr)
	require.NotNil(t, res)
	require.Equal(t, "provider_limit", res.ExitReason)
	require.EqualValues(t, 1, h.requests.Load())
	require.Len(t, res.FinishedWork, 1)
	require.Equal(t, "billing-command-output", res.FinishedWork[0].Result)
	require.Contains(t, wire, "billing-command-output")
	require.Contains(t, res.Error, "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits.")
	require.Contains(t, res.ResumeCommand, "--role smart --session "+h.sessionID)
	require.Empty(t, res.QuotaResetAt)
	jobs, err := h.app.asyncJobStore.ListAsyncJobsForOwner(ctx, h.sessionID)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, "completed", jobs[0].State)
	require.Equal(t, "pending", jobs[0].Delivery)
}
