package jobbatch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitCloseTimeoutFaultBoundary(t *testing.T) {
	t.Parallel()
	hookEntered, hookRelease := make(chan struct{}), make(chan struct{})
	closeEntered, closeResume := make(chan struct{}), make(chan struct{})
	r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			close(hookEntered)
			<-hookRelease
			return ErrCommitFailed
		}
		return nil
	}, &runnerRequestHooks{closeBeforeFeedLock: func() { close(closeEntered); <-closeResume }})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	go func() { _ = r.CapacityAvailable(ctx, "") }()
	runtimeTestReceive(t, ctx, hookEntered)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	reply := make(chan error, 1)
	go func() { reply <- r.Close(expired) }()
	runtimeTestReceive(t, ctx, closeEntered)
	close(hookRelease)
	runtimeTestReceive(t, ctx, r.Faulted())
	close(closeResume)
	err := runtimeTestReceive(t, ctx, reply)
	require.ErrorIs(t, err, ErrCommitFailed, "Close timeout masked concurrently signalled commit fault")
	require.NotErrorIs(t, err, ErrCloseTimeout)
	// Close returned without a termination report; only now confirm execution end.
	select {
	case <-b.Done():
		t.Fatal("fault published Done")
	default:
	}
	a.report(Result{State: StateCancelled})
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
}

func TestRuntimeCommitCloseTimeoutNoFaultControl(t *testing.T) {
	t.Parallel()
	for _, durable := range []bool{false, true} {
		name := "nil"
		if durable {
			name = "durable"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entered, resume := make(chan struct{}), make(chan struct{})
			hooks := &runnerRequestHooks{closeBeforeFeedLock: func() { close(entered); <-resume }}
			var r *Runner
			var e *runtimeTestExecutor
			var ctx context.Context
			if durable {
				r, e, _, ctx = runtimeTestCommitHooks(t, func(context.Context, Transition) error { return nil }, hooks)
			} else {
				r, e, _, ctx = runtimeTestNew(t, 1, 128, hooks)
			}
			b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
			expired, cancel := context.WithCancel(context.Background())
			cancel()
			reply := make(chan error, 1)
			go func() { reply <- r.Close(expired) }()
			runtimeTestReceive(t, ctx, entered)
			close(resume)
			err := runtimeTestReceive(t, ctx, reply)
			require.ErrorIs(t, err, ErrCloseTimeout)
			var timeout *CloseTimeoutError
			require.ErrorAs(t, err, &timeout)
			require.Equal(t, []ID{"r", "r.1"}, timeout.Nodes)
			a.report(Result{State: StateCancelled})
			runtimeTestWait(t, ctx, b)
			runtimeTestClose(t, ctx, r)
		})
	}
}
