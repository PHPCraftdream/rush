// R2C-15: a Drain iteration that fails in setup before any turn (no launch gate
// stands behind such a failure) is bounded, paced, visible on stderr and ends
// the run with exit reason "error", like the refusals the gate paces.
package app

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// runQueueFaultSessions fails the run-queue read ExecuteRun's setup performs
// (drainPendingBeforeRun) once armed: a persistent setup failure that only the
// Drain iterations see (the flag is armed after the first turn).
type runQueueFaultSessions struct {
	session.Service
	armed atomic.Bool
	calls atomic.Int64
}

var errRunQueueFault = errors.New("fx: run queue unreadable")

func (f *runQueueFaultSessions) HasOutstandingRunQueueEntriesForSession(ctx context.Context, sessionID string) (bool, error) {
	if f.armed.Load() {
		f.calls.Add(1)
		return false, errRunQueueFault
	}
	return f.Service.HasOutstandingRunQueueEntriesForSession(ctx, sessionID)
}

// Revert-check: without the setup-failure branch in afterDrain the failed
// Drain is neither paced nor bounded -- the loop relaunches back to back
// (thousands of setup attempts), never gives up on its own and this test goes
// red on the deadline assertion.
func TestRunNonInteractive_PersistentDrainSetupFailure_PacedBoundedVisible(t *testing.T) {
	origPause, origLimit := cliSetupRetryPause, cliLockBusyRetryOverallLimit
	cliSetupRetryPause, cliLockBusyRetryOverallLimit = 50*time.Millisecond, time.Second
	origErr := cliLoopStderr
	var stderr syncBuffer
	cliLoopStderr = &stderr
	t.Cleanup(func() {
		cliSetupRetryPause, cliLockBusyRetryOverallLimit, cliLoopStderr = origPause, origLimit, origErr
	})

	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			t.Error("a Drain that failed in setup must never reach the provider")
		}
		loopText(w, "a", "first answer", 11, 3)
	})
	fault := &runQueueFaultSessions{Service: h.app.Sessions}
	h.app.Sessions = fault
	h.afterFirstTurn(func() {
		h.seedDebt()
		fault.armed.Store(true)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	res, _, err := h.run(ctx, RunOverrides{})

	require.NoError(t, ctx.Err(), "the loop must give up on its own after the retry budget")
	require.ErrorIs(t, err, errRunQueueFault, "the run ends with the setup failure's own error")
	require.NotNil(t, res)
	require.Equal(t, "error", res.ExitReason)
	require.Equal(t, "first answer", res.FinalText, "a failed Drain never replaces the last completed answer")
	require.EqualValues(t, 1, h.requests.Load(), "only the first turn reached the provider")
	require.True(t, h.debtOpen(), "a setup failure never settles the debt")
	require.Zero(t, h.markers(), "a setup failure never writes a wake_failed marker")
	calls := fault.calls.Load()
	require.GreaterOrEqual(t, calls, int64(2), "the failed setup was retried")
	require.LessOrEqual(t, calls, int64(1000/50+10), "retries are paced (one per pause), not back to back")
	out := stderr.String()
	require.Contains(t, out, "could not be set up", "the first failure is visible on stderr")
	require.Contains(t, out, "giving up", "the give-up is visible on stderr")
	require.Contains(t, out, errRunQueueFault.Error())
}
