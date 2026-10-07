// Opt-in default background-job timeout (item 5): default termination of
// bash/run_command jobs that pass no explicit timeout, its precedence and
// its blast radius.
//
// Revert-check map — each test catches exactly one production line; revert
// it and the test must fail:
//   - TestDefaultJobTimeout_BashNoExplicitTimeoutEndsTimedOut:
//     async_tool.go Run's `if timeoutSpec == nil && (bash || run_command)`
//     block that builds the default terminate_and_wake TimeoutSpec.
//   - TestDefaultJobTimeout_ExplicitTimeoutBeatsDefault: the same block's
//     `timeoutSpec == nil` gate (an explicit per-call timeout must win).
//   - TestDefaultJobTimeout_NoDefaultArmsNoDeadline:
//     default_job_timeout.go resolveBackgroundJobDefaultTimeout's zero
//     return when neither CallOptions nor config set a default.
//   - TestDefaultJobTimeout_DelegationUntouchedByDefault: async_tool.go's
//     guard restricting the default to bash/run_command tool names.
//   - TestDefaultJobTimeout_CallOptionsWinOverConfig:
//     default_job_timeout.go's CallOptions-first precedence over config
//     Options.
package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// defaultTimeoutLedger wires an isolated ledger with a durable store and the
// real timeout service, the minimal fixture every async default-timeout test
// starts from (mirrors work_ledger_timeout_test.go).
func defaultTimeoutLedger(t *testing.T, onDone func(AsyncCompletion)) (*workLedger, *coordinator) {
	t.Helper()
	l := newWorkLedger(onDone)
	l.store = newTestAsyncJobStore(t)
	l.timeouts = newTimeoutService(l)
	t.Cleanup(l.timeouts.close)
	return l, &coordinator{asyncJobs: l, subAgentDrivers: newSubAgentDriverRegistry(), sessions: stubChildSessions{}}
}

// blockingInner is a stand-in bash/run_command process that blocks until its
// ctx is cancelled (the timeout kill path) or release closes.
func blockingInner(release <-chan struct{}) fantasy.AgentTool {
	return fantasy.NewAgentTool("bash", "Run a command", func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		select {
		case <-ctx.Done():
			return fantasy.ToolResponse{}, ctx.Err()
		case <-release:
			return fantasy.NewTextResponse("released output"), nil
		}
	})
}

// TestDefaultJobTimeout_BashNoExplicitTimeoutEndsTimedOut: a bash job with
// no timeout{} under a 2s CallOptions default reaches phaseTimedOut with a
// partial-output timed-out notice and a stopped executor (the kill path).
func TestDefaultJobTimeout_BashNoExplicitTimeoutEndsTimedOut(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 1)
	l, coord := defaultTimeoutLedger(t, func(c AsyncCompletion) { delivered <- c })
	release := make(chan struct{})
	defer close(release)
	inner := fantasy.NewAgentTool(tools.BashToolName, "Run a command", func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		<-ctx.Done()
		return fantasy.ToolResponse{}, ctx.Err()
	})
	wrapped := &asyncTool{inner: inner, coordinator: coord, name: tools.BashToolName}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginCLI)
	ctx = WithCallOptions(ctx, &CallOptions{BackgroundJobDefaultTimeout: 2 * time.Second})

	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: tools.BashToolName, Input: `{}`})
	require.NoError(t, err)
	require.Contains(t, resp.Content, "started")
	l.acknowledged(jobOf(l, "session", "call"))

	require.Eventually(t, func() bool {
		// deliverLocked deletes the job once it is terminal AND acknowledged,
		// so nil means the timed-out outcome was already delivered.
		job := jobOf(l, "session", "call")
		if job == nil {
			return true
		}
		// job.state is written under l.mu by the timeout service.
		l.mu.Lock()
		defer l.mu.Unlock()
		return job.state == phaseTimedOut
	}, 5*time.Second, 10*time.Millisecond, "the default deadline must terminate the job")
	select {
	case got := <-delivered:
		require.True(t, got.TimedOut)
		require.Equal(t, 2, got.TimeoutSeconds)
		require.NotEmpty(t, got.Content, "the timed-out notice must carry captured partial output")
	case <-time.After(2 * time.Second):
		t.Fatal("the timed-out completion was never delivered")
	}
	require.Eventually(t, func() bool { return !l.running("session") },
		2*time.Second, 10*time.Millisecond, "the executor must be cancelled (kill path) after termination")
}

// TestDefaultJobTimeout_ExplicitTimeoutBeatsDefault: a call carrying its own
// timeout{} wins over a (much shorter) operator default -- the wake_only
// deadline never terminates a sync job, so the caller sees the tool's real
// result, never a timed-out notice.
func TestDefaultJobTimeout_ExplicitTimeoutBeatsDefault(t *testing.T) {
	t.Parallel()
	_, coord := defaultTimeoutLedger(t, nil)
	// Inner finishes on its own after 400ms: past the 300ms default (if it
	// were wrongly applied) but never hitting the explicit 5s wake_only.
	inner := fantasy.NewAgentTool(tools.RunCommandToolName, "Run a program", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		time.Sleep(400 * time.Millisecond)
		return fantasy.NewTextResponse("done"), nil
	})
	wrapped := &asyncTool{inner: inner, coordinator: coord, name: tools.RunCommandToolName}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginSDK)
	ctx = WithCallOptions(ctx, &CallOptions{BackgroundJobDefaultTimeout: 300 * time.Millisecond})

	resp, err := wrapped.Run(ctx, fantasy.ToolCall{
		ID: "call", Name: tools.RunCommandToolName,
		Input: `{"timeout":{"seconds":5,"kind":"wake_only"}}`,
	})
	require.NoError(t, err)
	require.Contains(t, resp.Content, "done")
	require.NotContains(t, resp.Content, "timed out",
		"an explicit per-call timeout must win -- the 300ms default must never terminate this job")
}

// TestDefaultJobTimeout_NoDefaultArmsNoDeadline: with no default configured
// (CallOptions and config both silent), a bash job is started with NO
// deadline at all.
func TestDefaultJobTimeout_NoDefaultArmsNoDeadline(t *testing.T) {
	t.Parallel()
	completed := make(chan AsyncCompletion, 1)
	l, coord := defaultTimeoutLedger(t, func(c AsyncCompletion) { completed <- c })
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	wrapped := &asyncTool{inner: blockingInner(release), coordinator: coord, name: tools.BashToolName}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginCLI)

	_, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: tools.BashToolName, Input: `{}`})
	require.NoError(t, err)
	job := jobOf(l, "session", "call")
	require.NotNil(t, job)
	require.True(t, job.deadline.IsZero(), "no default configured: the job must have no deadline")
	require.Equal(t, timeoutKind(0), job.timeoutKind)

	releaseOnce.Do(func() { close(release) })
	l.acknowledged(job)
	select {
	case got := <-completed:
		require.False(t, got.TimedOut)
		require.Contains(t, got.Content, "released output")
	case <-time.After(2 * time.Second):
		t.Fatal("completion not delivered")
	}
}

// TestDefaultJobTimeout_DelegationUntouchedByDefault: an agentic_fetch
// (delegation) job is never armed with the default, even with a 2s default
// on the call.
func TestDefaultJobTimeout_DelegationUntouchedByDefault(t *testing.T) {
	t.Parallel()
	l, coord := defaultTimeoutLedger(t, nil)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	inner := fantasy.NewAgentTool(tools.AgenticFetchToolName, "Fetch", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		<-release
		return fantasy.NewTextResponse("fetched"), nil
	})
	wrapped := &asyncTool{inner: inner, coordinator: coord, name: tools.AgenticFetchToolName}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-1")
	ctx = WithCallOrigin(ctx, message.OriginCLI)
	ctx = WithCallOptions(ctx, &CallOptions{BackgroundJobDefaultTimeout: 2 * time.Second})

	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: tools.AgenticFetchToolName, Input: `{}`})
	require.NoError(t, err)
	require.Contains(t, resp.Content, "started")
	job := jobOf(l, "session", "call")
	require.NotNil(t, job)
	require.True(t, job.deadline.IsZero(), "a delegation must never inherit the bash/run_command default")
	releaseOnce.Do(func() { close(release) })
}

// TestDefaultJobTimeout_CallOptionsWinOverConfig: the run's CallOptions
// (`--job-timeout`) wins over config's
// background_job_default_timeout_seconds, which wins over "off".
func TestDefaultJobTimeout_CallOptionsWinOverConfig(t *testing.T) {
	// Not parallel: isolateAllGlobalConfigPaths uses t.Setenv.
	isolateAllGlobalConfigPaths(t)
	cfg, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	cfg.Config().Options.BackgroundJobDefaultTimeoutSeconds = 30
	coord := &coordinator{cfg: cfg}

	plain := context.Background()
	require.Equal(t, 30*time.Second, coord.resolveBackgroundJobDefaultTimeout(plain),
		"config alone must supply the default")

	perCall := WithCallOptions(plain, &CallOptions{BackgroundJobDefaultTimeout: 2 * time.Second})
	require.Equal(t, 2*time.Second, coord.resolveBackgroundJobDefaultTimeout(perCall),
		"CallOptions must win over config Options")
}
