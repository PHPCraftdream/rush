package app

import (
	"net/http"
	"os"
	"testing"
	"time"

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
	// Dual-mode seed: a live announced own command job (batch root flag ON,
	// legacy async_jobs row OFF).
	f6a2ClaimOwnCommandJob(t, ctx, h.app, h.sessionID, "billing-command")
	completed := make(chan error, 1)
	h.afterFirstTurn(func() {
		go func() {
			time.Sleep(100 * time.Millisecond)
			// The error-returning completion guarantees the channel always
			// receives the outcome, even when a fixture step fails.
			completed <- f6a2CompleteDelegationErr(ctx, h.app, h.sessionID, "billing-command", "billing-command-output", true)
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
	// Dual-mode readback (restored require.Len so a duplicate job is still
	// detected): exactly ONE completed own job, its completion pending
	// (never pulled).
	count, state, delivery := f6a2OwnJobReadback(t, h.app, h.sessionID, "billing-command")
	require.Equal(t, 1, count, "exactly one completed own job")
	require.Equal(t, "completed", state)
	require.Equal(t, "pending", delivery)
}
