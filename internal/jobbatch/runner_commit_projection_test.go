package jobbatch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitOwnerPinsAndRelease(t *testing.T) {
	t.Parallel()
	for _, op := range []runnerRequestOp{runnerRequestOutput, runnerRequestInject} {
		name := "output"
		if op == runnerRequestInject {
			name = "inject"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entered, release := make(chan struct{}), make(chan struct{})
			accepted := make(chan *runnerRequest, 16)
			settled := make(chan struct{}, 1)
			r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
				if tr.Event.Type == EventCapacityAvailable {
					close(entered)
					<-release
				}
				return nil
			}, &runnerRequestHooks{accepted: func(q *runnerRequest) { accepted <- q }, transitioned: func(tr Transition) {
				if tr.Event.Type == EventSettled {
					settled <- struct{}{}
				}
			}})
			b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
			disposed := make(chan struct{}, 1)
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{dispose: func(context.Context) error { disposed <- struct{}{}; return nil }})
			runtimeTestReceive(t, ctx, accepted)
			capacity := make(chan error, 1)
			go func() { capacity <- r.CapacityAvailable(ctx, "") }()
			runtimeTestReceive(t, ctx, entered)
			runtimeTestReceive(t, ctx, accepted)
			answer := make(chan runnerAnswer, 1)
			go func() { answer <- r.request(ctx, &runnerRequest{op: op, node: "r.1"}) }()
			runtimeTestReceive(t, ctx, accepted)
			barrier := make(chan error, 1)
			go func() { barrier <- r.Stop(ctx, "unknown") }()
			runtimeTestReceive(t, ctx, accepted)
			// The next accepted request is after pinning the inspection in owner order.
			r.mu.Lock()
			h := r.handles[0]
			r.mu.Unlock()
			h.mu.Lock()
			pins := h.pins
			h.mu.Unlock()
			require.Equal(t, 1, pins, "inspection pin delayed until publication")
			a.report(Result{State: StateCompleted})
			runtimeTestReceive(t, ctx, settled)
			released := make(chan error, 1)
			go func() { released <- r.Release(ctx, "r") }()
			runtimeTestReceive(t, ctx, accepted)
			// An additional request barriers after Release, which must give up ownership.
			go func() { _ = r.Stop(ctx, "unknown") }()
			runtimeTestReceive(t, ctx, accepted)
			h.mu.Lock()
			givenUp := h.givenUp
			h.mu.Unlock()
			require.True(t, givenUp, "Release ownership waited for commit")
			require.Empty(t, disposed, "Dispose overlapped undelivered inspection pin")
			select {
			case <-b.Done():
				t.Fatal("unpublished Settled closed Done")
			default:
			}
			close(release)
			require.NoError(t, runtimeTestReceive(t, ctx, capacity))
			got := runtimeTestReceive(t, ctx, answer)
			require.NoError(t, got.err)
			got.handle.unpin()
			require.ErrorIs(t, runtimeTestReceive(t, ctx, barrier), ErrUnknownNode)
			require.NoError(t, runtimeTestReceive(t, ctx, released))
			runtimeTestWait(t, ctx, b)
			runtimeTestReceive(t, ctx, disposed)
			_, err := r.View(ctx, "r")
			require.ErrorIs(t, err, ErrReleased)
			runtimeTestClose(t, ctx, r)
		})
	}
}

func TestRuntimeCommitOwnerCapturedRecords(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	accepted := make(chan struct{}, 4)
	r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventSubmit {
			close(entered)
			<-release
		}
		return nil
	}, &runnerRequestHooks{accepted: func(q *runnerRequest) {
		if q.event.Type == EventStopRequested {
			accepted <- struct{}{}
		}
	}})
	reply := make(chan *Batch, 1)
	go func() {
		b, _ := r.Submit(ctx, SubmitRequest{Root: "r", Spec: OneItem(TaskSpec{Kind: "test"})})
		reply <- b
	}()
	runtimeTestReceive(t, ctx, entered)
	stop := make(chan error, 1)
	go func() { stop <- r.Stop(ctx, "r") }()
	runtimeTestReceive(t, ctx, accepted)
	go func() { _ = r.Stop(ctx, "unknown") }()
	runtimeTestReceive(t, ctx, accepted)
	close(release)
	b := runtimeTestReceive(t, ctx, reply)
	require.NoError(t, runtimeTestReceive(t, ctx, stop))
	summary := runtimeTestWait(t, ctx, b)
	page := runtimeTestPage(t, ctx, r)
	require.Equal(t, StateRunning, page.Records[0].State, "record used later Engine state")
	require.Equal(t, StateCancelled, summary.State)
	require.Empty(t, e.starts, "stopped unpublished Start dispatched")
	runtimeTestClose(t, ctx, r)
}

func TestRuntimeCommitOwnerFinalGiveUp(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	r, e, _, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
		if tr.Final {
			close(entered)
			<-release
		}
		return nil
	})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	disposed := make(chan struct{}, 1)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{dispose: func(context.Context) error {
		r.feed.mu.Lock()
		final := r.feed.final
		r.feed.mu.Unlock()
		if final == 0 {
			t.Error("Dispose before Final publication")
		}
		disposed <- struct{}{}
		return nil
	}})
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, b)
	reply := make(chan error, 1)
	go func() { reply <- r.Close(ctx) }()
	runtimeTestReceive(t, ctx, entered)
	r.mu.Lock()
	h := r.handles[0]
	r.mu.Unlock()
	h.mu.Lock()
	givenUp := h.givenUp
	h.mu.Unlock()
	require.False(t, givenUp, "Final gave up at Apply")
	require.Empty(t, disposed)
	close(release)
	require.NoError(t, runtimeTestReceive(t, ctx, reply))
	runtimeTestReceive(t, ctx, disposed)
}

func TestRuntimeCommitOwnerBound(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	accepted := make(chan struct{}, runnerPublicationLimit+8)
	progress := make(chan EventType, 8)
	r, e, clock, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	}, &runnerRequestHooks{accepted: func(q *runnerRequest) {
		if q.event.Type == EventCapacityAvailable || (q.event.Type == EventSubmit && q.event.Root == "extra") {
			accepted <- struct{}{}
		}
	}, transitioned: func(tr Transition) {
		if tr.Event.Type == EventSettled || tr.Event.Type == EventDeadlineExpired {
			progress <- tr.Event.Type
		}
	}})
	drainDone := make(chan struct{})
	defer close(drainDone)
	go func() {
		for {
			select {
			case <-clock.armed:
			case <-drainDone:
				return
			}
		}
	}()
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test", Timeout: 10 * time.Second}))
	stopped := make(chan struct{}, 1)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return nil }})
	for range runnerPublicationLimit {
		go func() { _ = r.CapacityAvailable(ctx, "") }()
		runtimeTestReceive(t, ctx, accepted)
	}
	runtimeTestReceive(t, ctx, entered)
	extraCtx, cancel := context.WithCancel(ctx)
	extra := make(chan error, 1)
	go func() {
		_, err := r.Submit(extraCtx, SubmitRequest{Root: "extra", Spec: runtimeTestBatch(ModeParallel, NotifyAll)})
		extra <- err
	}()
	clock.mu.Lock()
	var deadline *runtimeTestTimer
	for _, timer := range clock.timers {
		if timer.at == 10*time.Second {
			deadline = timer
			break
		}
	}
	clock.mu.Unlock()
	require.NotNil(t, deadline)
	deadline.f()
	require.Equal(t, EventDeadlineExpired, runtimeTestReceive(t, ctx, progress))
	// Stop remains accepted independently of the bounded noncontainment queue.
	stop := make(chan error, 1)
	go func() { stop <- r.Stop(ctx, "r") }()
	runtimeTestReceive(t, ctx, stopped)
	require.Empty(t, accepted, "publication FIFO accepted beyond bound")
	cancel()
	require.ErrorIs(t, runtimeTestReceive(t, ctx, extra), context.Canceled)
	a.report(Result{State: StateCancelled})
	require.Equal(t, EventSettled, runtimeTestReceive(t, ctx, progress))
	close(release)
	require.NoError(t, runtimeTestReceive(t, ctx, stop))
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}
