package agent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAsyncToolReturnsBeforeCommandFinishes(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	inner := fantasy.NewAgentTool("run_command", "Run a program", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		close(started)
		<-release
		return fantasy.NewTextResponse("program output"), nil
	})
	// Phase-4 step 4: the in-memory ready queue is gone -- a CLI-origin
	// job's completion now routes through onWebDone exactly like a web
	// one (work_ledger.go's deliverLocked), so this test observes it the
	// same way TestAsyncToolWebCompletionWaitsForToolResult does.
	completed := make(chan AsyncCompletion, 1)
	registry := newWorkLedger(func(c AsyncCompletion) { completed <- c })
	registry.store = newTestAsyncJobStore(t)
	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginCLI)
	response, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: "run_command", Input: `{}`})
	require.NoError(t, err)
	require.Contains(t, response.Content, "started")
	require.Contains(t, response.Content, "end your turn", "start reply must tell the model not to wait with filler commands")
	var metadata asyncToolMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.True(t, metadata.Async)
	require.Equal(t, "call", metadata.JobID)
	<-started
	require.True(t, registry.running("session"))
	registry.acknowledged(jobOf(registry, "session", "call"))
	releaseOnce.Do(func() { close(release) })
	waitCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case completion := <-completed:
		require.Equal(t, "program output", completion.Content)
	case <-waitCtx.Done():
		t.Fatal("completion not delivered")
	}
	require.False(t, registry.running("session"))
}

// TestAsyncTool_InnerNeverRunsWhenStartFailsClosed strengthens DUR-8's
// coverage beyond workLedger.Start alone (TestWorkLedger_StartFailsClosedWhenStoreUnavailable,
// work_ledger_durable_test.go, whose own "executorStarted" bool is actually
// just its `cancel` callback -- Start never calls cancel on ANY path, so that
// assertion can never fail regardless of whether fail-closed actually works;
// review finding "executorStarted vacuous"). This proves the REAL property
// at the layer that owns it: asyncTool.Run must never reach `go t.run(...)`
// at all when Start fails, by observing the INNER tool itself never runs.
func TestAsyncTool_InnerNeverRunsWhenStartFailsClosed(t *testing.T) {
	t.Parallel()
	var innerRuns atomic.Int32
	inner := fantasy.NewAgentTool("bash", "Run a command", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		innerRuns.Add(1)
		return fantasy.NewTextResponse("should never run"), nil
	})
	registry := newWorkLedger(nil)
	registry.store = nil // DUR-8: no store wired -- Start must fail closed for a non-sync job
	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "bash"}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginCLI)

	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: "bash", Input: `{}`})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "store is unavailable")

	time.Sleep(50 * time.Millisecond) // would-be executor goroutine has had time to run if it were ever started
	require.EqualValues(t, 0, innerRuns.Load(), "the inner tool must never run when Start fails closed")
	require.False(t, registry.running("session"))
}

// TestAsyncTool_PanicBetweenStartAndExecutorLaunchFinalizesJob pins B15: a
// panic in the window between Start succeeding (the durable row already
// claimed, announced=0) and "go t.run" actually launching (here, forced via
// a permissions manager that panics) must not leave that row looking like a
// legitimately started, live job forever. Without the fix, asyncTool.Run's
// own panic propagates with no recovery at this layer, the executor never
// starts, yet the row stays claimed -- the NEXT tool result for this
// toolCallID (if the panic is caught and reported as an ordinary tool error
// further up the stack) would be fused by the ack gate as this job's own
// "started" ack, announcing a job nothing will ever finish.
//
// Revert-check performed: removed launchExecutor's defer/recover (reverting
// to the pre-fix inline body) -- this test FAILED with an unrecovered panic
// propagating out of wrapped.Run (crashing the test). Restored the recover;
// re-ran, passed. Diffed async_tool.go against git HEAD after restoring:
// matches the committed fix.
func TestAsyncTool_PanicBetweenStartAndExecutorLaunchFinalizesJob(t *testing.T) {
	t.Parallel()
	var innerRuns atomic.Int32
	inner := fantasy.NewAgentTool(tools.AgenticFetchToolName, "Fetch", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		innerRuns.Add(1)
		return fantasy.NewTextResponse("should never run either"), nil
	})
	delivered := make(chan AsyncCompletion, 1)
	registry := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	registry.store = newTestAsyncJobStore(t)
	coord := &coordinator{
		asyncJobs: registry, permissions: panickyPermissions{}, sessions: stubChildSessions{},
		// subAgentDrivers wired so childScopeDrained/recheckChild (finalize's
		// delegation path) don't nil-deref on a never-driven child session --
		// with no messages/background wired, refreshSubAgentCompletion and
		// ScopeOpen both degrade to "use the captured completion"/"no debt",
		// so recheckChild delivers the panic's own error completion straight
		// through, exactly as if the child scope were trivially drained
		// (which it is: nothing ever ran for it).
		subAgentDrivers: newSubAgentDriverRegistry(),
	}
	wrapped := &asyncTool{inner: inner, coordinator: coord, name: tools.AgenticFetchToolName}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-1")
	ctx = WithCallOrigin(ctx, message.OriginCLI)

	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: tools.AgenticFetchToolName, Input: `{}`})
	require.NoError(t, err, "the panic must be recovered here, not propagate out of Run")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "failed to start")

	// The row must already be terminal (failed) by the time Run returns --
	// finalize/transition ran synchronously inside launchExecutor's recover,
	// not left dangling at state='running' forever.
	require.Eventually(t, func() bool {
		row, err := registry.store.Get(context.Background(), "session", "call")
		return err == nil && row.State == "failed"
	}, 2*time.Second, 10*time.Millisecond, "the claimed row must reach a terminal state, not stay 'running' forever")

	// Delivery itself is withheld until the ack gate runs (job.announced),
	// exactly like any other job whose terminal transition commits before
	// its own "started" result is ever acknowledged (deliverLocked's
	// existing !job.announced guard, doc sec.3.8's Ack gate paragraph) --
	// Run returning gracefully (not panicking) means the caller's own
	// onToolResult WILL run next in production and acknowledge this exact
	// tool call, simulated here directly.
	select {
	case got := <-delivered:
		t.Fatalf("must not deliver before the ack gate runs: %+v", got)
	default:
	}
	registry.acknowledged(jobOf(registry, "session", "call"))

	waitCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case completion := <-delivered:
		require.True(t, completion.IsError)
	case <-waitCtx.Done():
		t.Fatal("the claimed row was never delivered once acknowledged -- it would stay unannounced forever")
	}
	require.EqualValues(t, 0, innerRuns.Load(), "the inner tool must never run -- the panic happened before go t.run")
	require.False(t, registry.running("session"))
}

// panickyPermissions is a minimal permission.Service whose
// InheritSessionAutoApprove always panics, simulating ANY panic in
// launchExecutor's window between Start and "go t.run" (B15) without
// depending on a specific real cause.
type panickyPermissions struct{ permission.Service }

func (panickyPermissions) InheritSessionAutoApprove(string, string) {
	panic("simulated panic before the executor launches")
}

// stubChildSessions is a minimal session.Service that only implements
// CreateAgentToolSessionID, enough to drive asyncTool.childSessionID's
// agentic_fetch branch without a real session store.
type stubChildSessions struct{ session.Service }

func (stubChildSessions) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return "child-session"
}

func TestAsyncToolWebCompletionWaitsForToolResult(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	inner := fantasy.NewAgentTool("run_command", "Run a program", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		<-release
		return fantasy.NewTextErrorResponse("exit 2"), nil
	})
	completed := make(chan AsyncCompletion, 1)
	registry := newWorkLedger(func(result AsyncCompletion) { completed <- result })
	registry.store = newTestAsyncJobStore(t)
	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginWeb)
	_, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: "run_command", Input: `{}`})
	require.NoError(t, err)
	releaseOnce.Do(func() { close(release) })
	// Phase-4 step 4: no in-memory ready queue to inspect any more -- "not
	// delivered before ack" is enforced structurally by deliverLocked's own
	// !job.announced guard, proven below by the completed channel staying
	// empty until acknowledged fires.
	select {
	case got := <-completed:
		t.Fatalf("must not deliver before acknowledged: %+v", got)
	default:
	}
	registry.acknowledged(jobOf(registry, "session", "call"))
	waitCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case result := <-completed:
		require.True(t, result.IsError)
		require.Equal(t, "exit 2", result.Content)
	case <-waitCtx.Done():
		t.Fatal("completion not delivered")
	}
}
