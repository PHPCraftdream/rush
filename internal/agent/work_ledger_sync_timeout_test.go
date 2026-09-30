// A synchronous (SDK/library) terminate_and_wake timeout (R2B-13): the ledger
// job has no row, so the timer must reach the timed-out outcome in memory --
// the blocked caller gets it with the partial output, not the executor's own
// "context canceled" that the timer's cancel provokes.
package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestWorkLedger_SyncTerminateAndWakeTimeoutYieldsTimedOutOutcomeWithPartialOutput
// arms a REAL timeout on a sync job and lets the timer service fire it.
//
// Revert-check: with handleTimeout sending a sync job through l.transition
// again (which skips sync jobs) the job never became terminal: awaitSync
// waited out its context and the test failed.
func TestWorkLedger_SyncTerminateAndWakeTimeoutYieldsTimedOutOutcomeWithPartialOutput(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.timeouts = newTimeoutService(l)
	t.Cleanup(l.close)
	cancelled := make(chan struct{})
	spec := &TimeoutSpec{Deadline: time.Now().Add(50 * time.Millisecond), Kind: timeoutTerminateAndWake, Seconds: 7}
	var cancelOnce sync.Once
	job, _, err := l.Start("owner", "call", "", "run_command", "", false, true, spec, func() { cancelOnce.Do(func() { close(cancelled) }) })
	require.NoError(t, err)
	buf := &fakeLiveOutputBuffer{}
	buf.write("partial text\n")
	l.setRunCommandBuffer(job, buf)

	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	res, err := l.awaitSync(ctx, job)
	require.NoError(t, err, "the timer must complete the sync job")
	require.True(t, res.isError)
	require.Contains(t, res.content, "timed out after 7s")
	require.Contains(t, res.content, "partial text")
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the executor was never asked to stop")
	}

	// The executor's own late return must not overwrite the outcome.
	l.finish(job, jobResult{content: "context canceled", isError: true})
	l.mu.Lock()
	defer l.mu.Unlock()
	require.Equal(t, phaseTimedOut, job.state)
	require.Contains(t, job.result.content, "timed out after 7s")
}

// TestAsyncTool_SyncTimeoutReturnsTimedOutResponseNotContextCanceled is the
// same through asyncTool.Run: the SDK caller's tool response.
func TestAsyncTool_SyncTimeoutReturnsTimedOutResponseNotContextCanceled(t *testing.T) {
	t.Parallel()
	inner := fantasy.NewAgentTool("run_command", "Run a program", func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		<-ctx.Done()
		return fantasy.ToolResponse{}, ctx.Err()
	})
	registry := newWorkLedger(nil)
	registry.store = newTestAsyncJobStore(t)
	registry.timeouts = newTimeoutService(registry)
	t.Cleanup(registry.close)
	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginSDK)

	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: "run_command", Input: `{"timeout_seconds":1}`})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "timed out after 1s and was stopped")
	require.NotContains(t, resp.Content, "context canceled")
	require.False(t, registry.running("session"))
}
