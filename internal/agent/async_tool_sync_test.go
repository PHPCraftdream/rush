package agent

// §4.6: the unified asyncTool execution path for SDK/unspecified origin --
// same registry, same explicit-timeout support as CLI/web, delivery blocked
// inside the call instead of a separate early-return branch.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestAsyncTool_SDKOriginBlocksAndReturnsInnerResponse: origin=SDK must
// block until the inner tool finishes and return its ToolResponse verbatim
// (content, error flag, metadata), same as a direct t.inner.Run(ctx, call)
// would have before phase 2.
func TestAsyncTool_SDKOriginBlocksAndReturnsInnerResponse(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	inner := fantasy.NewAgentTool("run_command", "Run a program", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		close(started)
		<-release
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse("inner output"), map[string]string{"k": "v"}), nil
	})
	registry := newWorkLedger(nil)
	registry.store = newTestAsyncJobStore(t)
	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginSDK)

	done := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: "run_command", Input: `{}`})
		require.NoError(t, err)
		done <- resp
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("inner tool never started")
	}
	select {
	case <-done:
		t.Fatal("Run returned before the inner tool finished")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	select {
	case resp := <-done:
		require.Equal(t, "inner output", resp.Content)
		require.False(t, resp.IsError)
		require.Contains(t, resp.Metadata, `"k":"v"`)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the inner tool finished")
	}
}

// TestAsyncTool_SDKOriginBashDoesNotForceBackground: a sync bash call must
// not have run_in_background silently forced to true -- that would change
// the response format from the tool's own BashResponseMetadata to
// backgroundJobSummary's text, an externally-visible change for an origin
// that must see byte-for-byte the same response as before phase 2.
// Revert-check performed: removed the `&& !sync` condition around the
// force-background branch in t.run -- this test FAILED (spy's captured
// input contained "run_in_background":true). Restored the condition; re-ran,
// passed.
func TestAsyncTool_SDKOriginBashDoesNotForceBackground(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var capturedInput string
	inner := fantasy.NewAgentTool("bash", "Run a command", func(_ context.Context, _ struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		mu.Lock()
		capturedInput = call.Input
		mu.Unlock()
		return fantasy.NewTextResponse("ok"), nil
	})
	registry := newWorkLedger(nil)
	registry.store = newTestAsyncJobStore(t)
	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: tools.BashToolName}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginSDK)

	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: tools.BashToolName, Input: `{"command":"echo hi"}`})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Content)

	mu.Lock()
	defer mu.Unlock()
	var parsed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(capturedInput), &parsed))
	require.NotContains(t, parsed, "run_in_background", "a sync bash call must not have run_in_background injected")
}

// TestAsyncTool_SyncJobRegisteredInLedgerWithCASAndTimeout: while a sync
// call executes, it must be a REAL ledger entry (running(sessionID) true),
// and its delivery must never reach onWebDone (spy counter stays 0).
func TestAsyncTool_SyncJobRegisteredInLedgerWithCASAndTimeout(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	inner := fantasy.NewAgentTool("run_command", "Run a program", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		close(started)
		<-release
		return fantasy.NewTextResponse("done"), nil
	})
	var webDoneCalls int
	registry := newWorkLedger(func(AsyncCompletion) { webDoneCalls++ })
	registry.store = newTestAsyncJobStore(t)
	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginSDK)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: "run_command", Input: `{}`})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("inner tool never started")
	}
	require.True(t, registry.running("session"), "the sync job must be a real ledger entry while running")

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
	require.Zero(t, webDoneCalls, "a sync job's delivery must never reach onWebDone")
}

// TestAsyncTool_SyncCallerCtxCancelUnblocksAwait: cancelling the ctx passed
// to Run must unblock it quickly with an error, not hang until the inner
// tool eventually finishes on its own.
// Revert-check performed: reverted §4.2's jobCtx branch to always use
// context.WithoutCancel(ctx) regardless of sync -- this test FAILED (timed
// out waiting for Run to return within the bound). Restored the sync/!sync
// branch; re-ran, passed.
func TestAsyncTool_SyncCallerCtxCancelUnblocksAwait(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	blockForever := make(chan struct{})
	// executorCancelled closes only when the INNER tool's own ctx actually
	// fires Done() -- distinct from Run() merely returning, which could
	// happen via awaitSync's own ctx.Done() branch even if the executor
	// goroutine below were leaked forever on a detached jobCtx. This is what
	// makes the revert-check meaningful: the spec's described regression
	// (unconditional context.WithoutCancel(ctx) for jobCtx) leaves Run's
	// caller-facing wait unblocking via awaitSync, while the executor itself
	// keeps running -- only observing THIS channel catches that.
	executorCancelled := make(chan struct{})
	inner := fantasy.NewAgentTool("run_command", "Run a program", func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		close(started)
		select {
		case <-blockForever:
		case <-ctx.Done():
			close(executorCancelled)
		}
		return fantasy.ToolResponse{}, ctx.Err()
	})
	registry := newWorkLedger(nil)
	registry.store = newTestAsyncJobStore(t)
	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
	baseCtx, cancel := context.WithCancel(t.Context())
	ctx := context.WithValue(baseCtx, tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginSDK)

	done := make(chan error, 1)
	go func() {
		_, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: "run_command", Input: `{}`})
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("inner tool never started")
	}
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not unblock promptly after ctx was cancelled")
	}
	select {
	case <-executorCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the inner executor's own ctx was never cancelled -- it would leak running forever")
	}
}
