package shell

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func heldExecution() (*BackgroundShell, *BackgroundExecution) {
	bs := &BackgroundShell{
		ID: "held", SessionID: "owner", cancel: func() {},
		stdout: newBoundedBuffer(1024), stderr: newBoundedBuffer(1024),
		done: make(chan struct{}), retained: true,
	}
	return bs, newBackgroundExecution(bs)
}

func TestBackgroundExecution_SerializedCancelAndEarlyDispose(t *testing.T) {
	bs, execution := heldExecution()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var calls atomic.Int32
	bs.cancel = func() {
		calls.Add(1)
		entered <- struct{}{}
		<-release
	}
	_, err := bs.stdout.WriteString("still owned")
	require.NoError(t, err)
	firstCtx, firstCancel := context.WithCancel(t.Context())
	defer firstCancel()
	first := make(chan error, 1)
	go func() { first <- execution.Stop(firstCtx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first cancel did not enter")
	}

	disposed := make(chan error, 1)
	go func() { disposed <- execution.Dispose(t.Context()) }()
	select {
	case err := <-disposed:
		require.ErrorIs(t, err, ErrExecutionNotDone, "early Dispose must reject while Stop is physically held")
	case <-time.After(time.Second):
		t.Fatal("early Dispose blocked behind Stop")
	}
	require.False(t, bs.bufReleased.Load(), "early Dispose must not release output")

	secondCtx, secondCancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer secondCancel()
	second := make(chan error, 1)
	go func() { second <- execution.Stop(secondCtx) }()
	select {
	case err := <-second:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("queued Stop did not honor its context")
	}
	require.EqualValues(t, 1, calls.Load(), "cancel dispatch must not overlap the held first call")
	firstCancel()
	unblock()
	require.ErrorIs(t, <-first, context.Canceled)
	require.False(t, bs.IsDone(), "cancel return is not completion")

	// A later call may dispatch only after the first physically returned.
	retryCtx, retryCancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer retryCancel()
	require.ErrorIs(t, execution.Stop(retryCtx), context.DeadlineExceeded)
	require.EqualValues(t, 2, calls.Load(), "retry may dispatch after actual cancel return")
	close(bs.done)
	require.NoError(t, execution.Dispose(t.Context()))
	require.True(t, bs.bufReleased.Load())
}

func TestBackgroundExecution_OutputCountersAndWait(t *testing.T) {
	bs, execution := heldExecution()
	payload := "HEAD\n" + strings.Repeat("padding", 1000) + "\nTAIL"
	_, err := bs.stdout.WriteString(payload)
	require.NoError(t, err)
	_, err = bs.stderr.WriteString("error stream")
	require.NoError(t, err)
	stdout, stderr, done, err := execution.GetOutput()
	require.NoError(t, err)
	require.False(t, done)
	require.Contains(t, stdout, "HEAD")
	require.Contains(t, stdout, "TAIL")
	require.Contains(t, stdout, "bytes truncated")
	require.LessOrEqual(t, len(stdout), 1024)
	require.Equal(t, "error stream", stderr)
	count, err := execution.TotalWrittenBytes()
	require.NoError(t, err)
	require.Equal(t, len(payload)+len(stderr), count)

	waitCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, execution.WaitForChange(waitCtx, count), context.DeadlineExceeded)
	_, err = bs.stdout.WriteString(" growth")
	require.NoError(t, err)
	growthCtx, growthCancel := context.WithTimeout(t.Context(), time.Second)
	defer growthCancel()
	require.NoError(t, execution.WaitForChange(growthCtx, count), "overflow growth must use total bytes, not snapshot length")
	close(bs.done)
	require.NoError(t, execution.WaitForChange(t.Context(), count+1000), "completion must unblock output wait")
	require.NoError(t, execution.Dispose(t.Context()))
}

func TestBackgroundExecution_ManagerTimeoutRetainedVersusLegacy(t *testing.T) {
	for _, retained := range []bool{false, true} {
		name := "legacy"
		if retained {
			name = "retained"
		}
		t.Run(name, func(t *testing.T) {
			manager := newBackgroundShellManager()
			bs, execution := heldExecution()
			bs.retained = retained
			manager.shells.Set(bs.ID, bs)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			require.ErrorIs(t, manager.KillOwned(ctx, "owner", bs.ID), context.DeadlineExceeded)
			require.False(t, bs.IsDone(), "manager timeout must leave real done open")
			_, ok := manager.Get(bs.ID)
			require.False(t, ok)
			_, err := bs.stdout.WriteString("final")
			require.NoError(t, err)
			bs.completedAt.Store(time.Now().Unix())
			bs.armBufferReleaseTimer(time.Hour)
			close(bs.done)
			require.Equal(t, !retained, bs.bufReleased.Load(), "only opt-in execution may retain detached output")
			if retained {
				stdout, _, done, err := execution.GetOutput()
				require.NoError(t, err)
				require.True(t, done)
				require.Equal(t, "final", stdout)
				require.NoError(t, execution.Dispose(t.Context()))
			}
		})
	}
}

func TestBackgroundExecution_NoRetentionTimersBeforeDispose(t *testing.T) {
	bs, execution := heldExecution()
	_, err := bs.stdout.WriteString("owned output")
	require.NoError(t, err)
	bs.completedAt.Store(time.Now().Unix())
	close(bs.done)
	bs.armBufferReleaseTimer(time.Millisecond)
	bs.detachFromManager()
	bs.retentionMu.Lock()
	bs.armDetachedReleaseTimerLocked()
	retentionTimer, detachedTimer := bs.retentionTimer, bs.detachedReleaseTimer
	bs.retentionMu.Unlock()
	require.Nil(t, retentionTimer, "retained launch must not arm timed release")
	require.Nil(t, detachedTimer, "retained detach must not arm timed release")
	bs.releaseBuffers()
	require.False(t, bs.bufReleased.Load(), "all automatic release paths must respect ownership")
	stdout, _, _, err := execution.GetOutput()
	require.NoError(t, err)
	require.Equal(t, "owned output", stdout)
	require.NoError(t, execution.Dispose(t.Context()))
}
