package shell

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRetainedOutputAbsoluteRetentionAndEOF(t *testing.T) {
	bs, e := heldExecution()
	o := newRetainedOutput(5)
	e.physical = &retainedStops{output: o}
	_, err := o.Write([]byte("abcdef"))
	require.NoError(t, err)
	chunk, err := e.ReadOutput(t.Context(), 0)
	require.NoError(t, err)
	require.Equal(t, "bcdef", string(chunk.Data))
	require.EqualValues(t, 1, chunk.Oldest)
	require.EqualValues(t, 6, chunk.Next)
	require.True(t, chunk.Truncated)
	require.False(t, chunk.EOF)
	chunk.Data[0] = 'X'
	close(bs.done)
	chunk, err = e.ReadOutput(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "bcdef", string(chunk.Data))
	require.True(t, chunk.EOF)
	require.NoError(t, e.Dispose(t.Context()))
	require.Empty(t, o.data)
	_, err = e.ReadOutput(t.Context(), 6)
	require.ErrorIs(t, err, ErrExecutionDisposed)
}

func TestRetainedOutputEventWaitAndCancel(t *testing.T) {
	bs, e := heldExecution()
	o := newRetainedOutput(10)
	e.physical = &retainedStops{output: o}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := e.ReadOutput(ctx, 0)
	require.ErrorIs(t, err, context.Canceled)
	result := make(chan ExecutionOutput, 1)
	go func() { c, _ := e.ReadOutput(t.Context(), 0); result <- c }()
	_, err = o.Write([]byte("event"))
	require.NoError(t, err)
	require.Equal(t, "event", string((<-result).Data))
	close(bs.done)
	require.NoError(t, e.Dispose(t.Context()))
}

func TestRetainedOwnedMergedOutputSurvivesRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	m := NewBackgroundShellManager()
	e, err := m.StartRetainedOwned(ctx, "owner", t.TempDir(), nil, "printf out; printf err >&2", "")
	require.NoError(t, err)
	require.NoError(t, m.RemoveOwned("owner", e.ID()))
	var data []byte
	var cursor uint64
	for {
		c, err := e.ReadOutput(ctx, cursor)
		require.NoError(t, err)
		data = append(data, c.Data...)
		cursor = c.Next
		if c.EOF {
			break
		}
	}
	require.Equal(t, "outerr", string(data))
	require.NoError(t, e.Dispose(ctx))
}

func TestRetainedOwnedStopAfterOutputSynchronization(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	m := NewBackgroundShellManager()
	e, err := m.StartRetainedOwned(ctx, "owner", t.TempDir(), nil, "printf ready; sleep 30", "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	c, err := e.ReadOutput(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, "ready", string(c.Data))
	require.NoError(t, e.Stop(ctx))
	require.NoError(t, e.Stop(ctx))
	select {
	case <-e.Done():
	case <-ctx.Done():
		t.Fatal("retained stop did not reach real completion")
	}
	require.NoError(t, e.Dispose(ctx))
}

func TestRetainedStopBeforeStartupRejectsSpawn(t *testing.T) {
	s := &retainedStops{entries: make(map[*retainedProcess]struct{})}
	require.NoError(t, s.stop())
	require.True(t, s.cancelled)
	// A latched invocation cancellation is checked before touching cmd.
	require.ErrorIs(t, runRetainedProcess(t.Context(), nil, s, nil, nil), context.Canceled)
}
