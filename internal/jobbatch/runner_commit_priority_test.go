package jobbatch

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitRequestEntryFaultPriority(t *testing.T) {
	t.Parallel()
	for _, op := range []runnerRequestOp{runnerRequestEvent, runnerRequestView, runnerRequestSnapshot} {
		name := "mutation"
		if op == runnerRequestView {
			name = "view"
		}
		if op == runnerRequestSnapshot {
			name = "snapshot"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for range 300 {
				entered, release := make(chan struct{}), make(chan struct{})
				paused, resume := make(chan struct{}), make(chan struct{})
				var armed atomic.Bool
				r, _, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
					if tr.Event.Type == EventCapacityAvailable {
						close(entered)
						<-release
						return ErrCommitFailed
					}
					return nil
				}, &runnerRequestHooks{beforeRequestSubmit: func() {
					if armed.Load() {
						close(paused)
						<-resume
					}
				}})
				b := runtimeTestSubmit(t, ctx, r, "r", runtimeTestBatch(ModeParallel, NotifyAll))
				runtimeTestWait(t, ctx, b)
				go func() { _ = r.CapacityAvailable(ctx, "") }()
				runtimeTestReceive(t, ctx, entered)
				armed.Store(true)
				reply := make(chan runnerAnswer, 1)
				go func() {
					reply <- r.request(ctx, &runnerRequest{op: op, node: "r", event: Event{Type: EventStopRequested, Node: "r"}})
				}()
				runtimeTestReceive(t, ctx, paused)
				close(release)
				runtimeTestReceive(t, ctx, r.Faulted())
				runtimeTestReceive(t, ctx, r.exited)
				close(resume)
				require.ErrorIs(t, runtimeTestReceive(t, ctx, reply).err, ErrCommitFailed, "unanswered request escaped fault through exited path")
			}
		})
	}
}

func TestRuntimeCommitWaitFaultContextPriority(t *testing.T) {
	t.Parallel()
	r, e, _, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			return ErrCommitFailed
		}
		return nil
	})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	require.ErrorIs(t, r.CapacityAvailable(ctx, ""), ErrCommitFailed)
	runtimeTestReceive(t, ctx, r.Faulted())
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	for range 300 {
		_, err := b.Wait(expired)
		require.ErrorIs(t, err, ErrCommitFailed, "Wait cancellation masked recorded fault")
	}
	a.report(Result{State: StateCancelled})
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
}

func TestRuntimeCommitEventsFaultContextPriority(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	paused, resume := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	r, _, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			close(entered)
			<-release
			return ErrCommitFailed
		}
		return nil
	}, &runnerRequestHooks{beforeEventsContext: func() {
		if armed.CompareAndSwap(true, false) {
			close(paused)
			<-resume
		}
	}})
	runtimeTestSubmit(t, ctx, r, "r", runtimeTestBatch(ModeParallel, NotifyAll))
	page := runtimeTestPage(t, ctx, r)
	go func() { _ = r.CapacityAvailable(ctx, "") }()
	runtimeTestReceive(t, ctx, entered)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	armed.Store(true)
	reply := make(chan error, 1)
	go func() { _, err := r.Events(expired, page.Next); reply <- err }()
	runtimeTestReceive(t, ctx, paused)
	close(release)
	runtimeTestReceive(t, ctx, r.Faulted())
	close(resume)
	require.ErrorIs(t, runtimeTestReceive(t, ctx, reply), ErrCommitFailed, "Events cancellation masked recorded fault")
	for range 300 {
		_, err := r.Events(expired, page.Next)
		require.ErrorIs(t, err, ErrCommitFailed)
	}
}

func TestRuntimeCommitPhysicalOutputFaultPriority(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	r, e, _, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			return ErrCommitFailed
		}
		return nil
	})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{read: func(context.Context, uint64) (OutputChunk, error) {
		close(entered)
		<-release
		return OutputChunk{EOF: true}, nil
	}})
	reply := make(chan error, 1)
	go func() { _, err := r.Output(ctx, "r.1", 0); reply <- err }()
	runtimeTestReceive(t, ctx, entered)
	require.ErrorIs(t, r.CapacityAvailable(ctx, ""), ErrCommitFailed)
	runtimeTestReceive(t, ctx, r.Faulted())
	close(release)
	require.ErrorIs(t, runtimeTestReceive(t, ctx, reply), ErrCommitFailed, "physical Output success escaped recorded fault")
	a.report(Result{State: StateCancelled})
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
	select {
	case <-b.Done():
		t.Fatal("fault published Done")
	default:
	}
}
