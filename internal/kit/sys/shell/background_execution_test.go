package shell

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBackgroundExecution_TimeoutRemovalLateDone(t *testing.T) {
	manager := newBackgroundShellManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bs := &BackgroundShell{
		ID: "held", SessionID: "owner", ctx: ctx, cancel: cancel,
		stdout: newBoundedBuffer(1024), stderr: newBoundedBuffer(1024),
		done: make(chan struct{}), retained: true,
	}
	manager.shells.Set(bs.ID, bs)
	execution := newBackgroundExecution(bs)
	require.Equal(t, (<-chan struct{})(bs.done), execution.Done())
	require.ErrorIs(t, execution.Dispose(t.Context()), ErrExecutionNotDone)

	stopCtx, stopCancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer stopCancel()
	require.ErrorIs(t, execution.Stop(stopCtx), context.DeadlineExceeded)
	require.ErrorIs(t, ctx.Err(), context.Canceled, "Stop must request cancellation")
	require.False(t, bs.IsDone(), "control timeout must not synthesize Done")
	require.NoError(t, manager.RemoveOwned("owner", bs.ID))
	_, found := manager.Get(bs.ID)
	require.False(t, found)

	retryCtx, retryCancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer retryCancel()
	require.ErrorIs(t, execution.Stop(retryCtx), context.DeadlineExceeded,
		"Stop must remain usable after manager removal")
	_, err := bs.stdout.WriteString("late final output")
	require.NoError(t, err)
	bs.completedAt.Store(time.Now().Unix())
	bs.armBufferReleaseTimer(time.Millisecond)
	close(bs.done)

	require.NoError(t, execution.Stop(t.Context()))
	stdout, _, done, err := execution.GetOutput()
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, "late final output", stdout, "detachment must not release retained output")
	bs.releaseBuffers()
	require.False(t, bs.bufReleased.Load(), "automatic release must respect retained ownership")
	count, err := execution.TotalWrittenBytes()
	require.NoError(t, err)
	require.Equal(t, len(stdout), count)
	require.NoError(t, execution.Dispose(t.Context()))
	require.NoError(t, execution.Dispose(t.Context()))
	require.True(t, bs.bufReleased.Load())
	_, _, _, err = execution.GetOutput()
	require.ErrorIs(t, err, ErrExecutionDisposed)
}

func TestBackgroundExecution_RetainedLaunch(t *testing.T) {
	manager := newBackgroundShellManager()
	execution, err := manager.StartRetainedOwned(t.Context(), "owner", t.TempDir(), nil, "echo retained", "")
	require.NoError(t, err)
	select {
	case <-execution.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("real execution did not complete")
	}
	require.NoError(t, manager.RemoveOwned("owner", execution.ID()))
	stdout, _, done, err := execution.GetOutput()
	require.NoError(t, err)
	require.True(t, done)
	require.Contains(t, stdout, "retained")
	require.NoError(t, execution.Dispose(t.Context()))
}
