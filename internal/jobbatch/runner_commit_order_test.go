package jobbatch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitFinalExactPublication(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	var r *Runner
	var b *Batch
	doneSeen := make(chan struct{}, 1)
	disposed := make(chan struct{}, 1)
	var e *runtimeTestExecutor
	var ctx context.Context
	r, e, _, ctx = runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Final {
			close(entered)
			<-release
		}
		return nil
	}, &runnerRequestHooks{donePublished: func(batch *Batch, tr Transition) {
		if !tr.Final {
			return
		}
		page := runtimeTestPage(t, context.Background(), r)
		require.NotZero(t, page.Final)
		require.Equal(t, RecordClosed, page.Records[len(page.Records)-1].Type)
		require.Equal(t, tr.Sequence, page.Records[len(page.Records)-1].Sequence)
		require.False(t, r.isClosed(), "Closed before Done publication")
		require.Empty(t, disposed, "Dispose before Done")
		require.Equal(t, batch, b)
		doneSeen <- struct{}{}
	}})
	b = runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	stopped := make(chan struct{}, 1)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return nil }, dispose: func(context.Context) error {
		require.True(t, r.isClosed(), "Dispose before Closed")
		select {
		case <-r.exited:
		default:
			t.Error("Dispose before owner exit")
		}
		select {
		case <-b.Done():
		default:
			t.Error("Dispose before Done")
		}
		disposed <- struct{}{}
		return nil
	}})
	reply := make(chan error, 1)
	go func() { reply <- r.Close(ctx) }()
	runtimeTestReceive(t, ctx, stopped)
	a.report(Result{State: StateCancelled})
	runtimeTestReceive(t, ctx, entered)
	require.False(t, r.isClosed())
	require.Empty(t, disposed)
	close(release)
	runtimeTestReceive(t, ctx, doneSeen)
	require.NoError(t, runtimeTestReceive(t, ctx, reply))
	runtimeTestReceive(t, ctx, disposed)
}

func TestRuntimeCommitRejectionAndPinFault(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	accepted := make(chan *runnerRequest, 16)
	settled := make(chan struct{}, 1)
	r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			close(entered)
			<-release
			return ErrCommitFailed
		}
		return nil
	}, &runnerRequestHooks{accepted: func(q *runnerRequest) { accepted <- q }, transitioned: func(tr Transition) {
		if tr.Event.Type == EventSettled {
			settled <- struct{}{}
		}
	}})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	runtimeTestReceive(t, ctx, accepted)
	disposed := make(chan struct{}, 1)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{dispose: func(context.Context) error { disposed <- struct{}{}; return nil }})
	go func() { _ = r.CapacityAvailable(ctx, "") }()
	runtimeTestReceive(t, ctx, entered)
	runtimeTestReceive(t, ctx, accepted)
	output := make(chan error, 1)
	go func() { _, err := r.Output(ctx, "r.1", 0); output <- err }()
	runtimeTestReceive(t, ctx, accepted)
	a.report(Result{State: StateCompleted})
	runtimeTestReceive(t, ctx, settled)
	terminal := make(chan error, 1)
	go func() {
		_, err := r.Add(ctx, AddRequest{Parent: "r", Items: []Spec{runtimeTestTask()}})
		terminal <- err
	}()
	runtimeTestReceive(t, ctx, accepted)
	released := make(chan error, 1)
	go func() { released <- r.Release(ctx, "r") }()
	runtimeTestReceive(t, ctx, accepted)
	repeated := make(chan error, 1)
	go func() { repeated <- r.Release(ctx, "r") }()
	runtimeTestReceive(t, ctx, accepted)
	require.Empty(t, disposed)
	close(release)
	for _, answer := range []<-chan error{output, terminal, released, repeated} {
		require.ErrorIs(t, runtimeTestReceive(t, ctx, answer), ErrCommitFailed)
	}
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
	runtimeTestReceive(t, ctx, disposed)
	select {
	case <-b.Done():
		t.Fatal("unpublished settlement Done closed")
	default:
	}
}

func TestRuntimeCommitFaultSignalStopsApply(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	queued, resume := make(chan struct{}), make(chan struct{})
	transitions := make(chan EventType, 16)
	r, _, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			close(entered)
			<-release
			return ErrCommitFailed
		}
		return nil
	}, &runnerRequestHooks{queued: func(q *runnerRequest) {
		if q.event.Type == EventSubmit && q.event.Root == "later" {
			close(queued)
			<-resume
		}
	}, transitioned: func(tr Transition) { transitions <- tr.Event.Type }})
	runtimeTestSubmit(t, ctx, r, "r", runtimeTestBatch(ModeParallel, NotifyAll))
	go func() { _ = r.CapacityAvailable(ctx, "") }()
	runtimeTestReceive(t, ctx, entered)
	later := make(chan error, 1)
	go func() {
		_, err := r.Submit(ctx, SubmitRequest{Root: "later", Spec: runtimeTestBatch(ModeParallel, NotifyAll)})
		later <- err
	}()
	runtimeTestReceive(t, ctx, queued)
	close(release)
	runtimeTestReceive(t, ctx, r.Faulted())
	close(resume)
	require.ErrorIs(t, runtimeTestReceive(t, ctx, later), ErrCommitFailed)
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
	require.Len(t, transitions, 2, "Engine Apply after commit fault signal")
}
