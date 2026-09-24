package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAsyncJobRegistryWaitsForPersistedToolResult(t *testing.T) {
	t.Parallel()
	r := newAsyncJobRegistry(nil)
	require.NoError(t, r.start("session", "call", true, nil))
	want := AsyncCompletion{SessionID: "session", ToolCallID: "call", ToolName: "bash", Content: "done"}
	r.finish(want)

	r.mu.Lock()
	ready, pending := len(r.sessions["session"].ready), len(r.sessions["session"].jobs)
	r.mu.Unlock()
	require.Zero(t, ready)
	require.Equal(t, 1, pending)

	r.acknowledged("session", "call")
	got, ok, err := r.next(t.Context(), "session")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, got)
	_, ok, err = r.next(t.Context(), "session")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestAsyncJobRegistryWebCallbackExactlyOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	r := newAsyncJobRegistry(func(got AsyncCompletion) {
		require.Equal(t, "call", got.ToolCallID)
		calls.Add(1)
	})
	require.NoError(t, r.start("session", "call", false, nil))
	r.acknowledged("session", "call")
	r.finish(AsyncCompletion{SessionID: "session", ToolCallID: "call"})
	r.finish(AsyncCompletion{SessionID: "session", ToolCallID: "call"})
	require.EqualValues(t, 1, calls.Load())
	_, ok, err := r.next(t.Context(), "session")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestAsyncJobRegistryConcurrentFinishAndAcknowledge(t *testing.T) {
	t.Parallel()
	r := newAsyncJobRegistry(nil)
	require.NoError(t, r.start("session", "call", true, nil))
	var wg sync.WaitGroup
	wg.Go(func() { r.finish(AsyncCompletion{SessionID: "session", ToolCallID: "call"}) })
	wg.Go(func() { r.acknowledged("session", "call") })
	wg.Wait()
	_, ok, err := r.next(t.Context(), "session")
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = r.next(t.Context(), "session")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestAsyncJobRegistryCancelAndClose(t *testing.T) {
	t.Parallel()
	r := newAsyncJobRegistry(nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, r.start("session", "call", true, cancel))
	waitCtx, stopWaiting := context.WithCancel(t.Context())
	stopWaiting()
	_, _, err := r.next(waitCtx, "session")
	require.ErrorIs(t, err, context.Canceled)
	r.close()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	_, ok, err := r.next(t.Context(), "session")
	require.NoError(t, err)
	require.False(t, ok)
	require.ErrorContains(t, r.start("session", "new-call", true, nil), "closed")
}
