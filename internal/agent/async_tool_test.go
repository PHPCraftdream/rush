package agent

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
	registry := newAsyncJobRegistry(nil)
	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginCLI)
	response, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: "run_command", Input: `{}`})
	require.NoError(t, err)
	require.Contains(t, response.Content, "started")
	var metadata asyncToolMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.True(t, metadata.Async)
	require.Equal(t, "call", metadata.JobID)
	<-started
	require.True(t, registry.pending("session"))
	registry.acknowledged("session", "call")
	releaseOnce.Do(func() { close(release) })
	waitCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	completion, ok, err := registry.next(waitCtx, "session")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "program output", completion.Content)
	require.False(t, registry.pending("session"))
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
	registry := newAsyncJobRegistry(func(result AsyncCompletion) { completed <- result })
	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: registry}, name: "run_command"}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session")
	ctx = WithCallOrigin(ctx, message.OriginWeb)
	_, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call", Name: "run_command", Input: `{}`})
	require.NoError(t, err)
	releaseOnce.Do(func() { close(release) })
	registry.mu.Lock()
	ready := len(registry.sessions["session"].ready)
	registry.mu.Unlock()
	require.Zero(t, ready)
	registry.acknowledged("session", "call")
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
