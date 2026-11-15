package jobbatch

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitOwnerFinalFence(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "fault"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entered, release := make(chan struct{}), make(chan struct{})
			accepted := make(chan struct{}, 1)
			r, _, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
				if tr.Final {
					close(entered)
					<-release
					if fail {
						return ErrCommitFailed
					}
				}
				return nil
			}, &runnerRequestHooks{accepted: func(q *runnerRequest) {
				if q.event.Type == EventStopRequested {
					accepted <- struct{}{}
				}
			}})
			runtimeTestSubmit(t, ctx, r, "r", runtimeTestBatch(ModeParallel, NotifyAll))
			closeReply := make(chan error, 1)
			go func() { closeReply <- r.Close(ctx) }()
			runtimeTestReceive(t, ctx, entered)
			stop := make(chan error, 1)
			go func() { stop <- r.Stop(ctx, "r") }()
			runtimeTestReceive(t, ctx, accepted)
			select {
			case <-stop:
				t.Fatal("Final rejection escaped publication fence")
			default:
			}
			close(release)
			if fail {
				require.ErrorIs(t, runtimeTestReceive(t, ctx, stop), ErrCommitFailed)
				require.ErrorIs(t, runtimeTestReceive(t, ctx, closeReply), ErrCommitFailed)
			} else {
				require.ErrorIs(t, runtimeTestReceive(t, ctx, stop), ErrClosed)
				require.NoError(t, runtimeTestReceive(t, ctx, closeReply))
			}
		})
	}
}

func TestRuntimeCommitOwnerDeadlineProgress(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	r, e, c, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			close(entered)
			<-release
		}
		return nil
	})
	defer close(release)
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test", Timeout: 10 * time.Second}))
	stopped := make(chan struct{}, 1)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return nil }})
	go func() { _ = r.CapacityAvailable(ctx, "") }()
	runtimeTestReceive(t, ctx, entered)
	// Fire only the attempt timer, leaving the unrelated hook held.
	c.mu.Lock()
	var timer *runtimeTestTimer
	for _, candidate := range c.timers {
		if candidate.at == 10*time.Second && candidate.active {
			timer = candidate
			break
		}
	}
	c.mu.Unlock()
	require.NotNil(t, timer)
	timer.f()
	runtimeTestCommitStopEntered(t, ctx, stopped, "deadline containment waited for unrelated commit")
	runtimeTestReceive(t, ctx, a.ctx.Done())
	release <- struct{}{}
	a.report(Result{State: StateCancelled})
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}

func TestRuntimeCommitOwnerUnpublishedStartStop(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	accepted := make(chan struct{}, 2)
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
	submitted := make(chan *Batch, 1)
	go func() {
		b, _ := r.Submit(ctx, SubmitRequest{Root: "r", Spec: OneItem(TaskSpec{Kind: "test"})})
		submitted <- b
	}()
	runtimeTestReceive(t, ctx, entered)
	stop := make(chan error, 1)
	go func() { stop <- r.Stop(ctx, "r.1") }()
	runtimeTestReceive(t, ctx, accepted)
	// A second accepted Stop is an owner-order barrier after the first Apply.
	go func() { _ = r.Stop(ctx, "r.1") }()
	runtimeTestReceive(t, ctx, accepted)
	close(release)
	b := runtimeTestReceive(t, ctx, submitted)
	require.NoError(t, runtimeTestReceive(t, ctx, stop))
	select {
	case a := <-e.starts:
		require.Error(t, a.ctx.Err(), "unpublished stopped Start entered live")
		a.decision <- runtimeTestDecision{err: ErrExecutionDone}
	default:
	}
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}

func TestRuntimeCommitOwnerQueuedStopProgress(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	accepted := make(chan struct{}, 4)
	returned := make(chan struct{}, 4)
	stopEvents := 0
	r, e, c, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventStopRequested {
			stopEvents++
			if stopEvents == 2 {
				close(entered)
				<-release
			}
		}
		return nil
	}, &runnerRequestHooks{accepted: func(q *runnerRequest) {
		if q.event.Type == EventStopRequested {
			accepted <- struct{}{}
		}
	}, controlReturned: func() { returned <- struct{}{} }})
	defer close(release)
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	stopped := make(chan struct{}, 4)
	returns := make(chan error, 4)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return <-returns }})
	require.NoError(t, r.Stop(ctx, "r.1"))
	runtimeTestReceive(t, ctx, accepted)
	runtimeTestReceive(t, ctx, stopped)
	c.mu.Lock()
	var timer *runtimeTestTimer
	for _, candidate := range c.timers {
		if candidate.at == 50*time.Second && candidate.active {
			timer = candidate
			break
		}
	}
	c.mu.Unlock()
	require.NotNil(t, timer)
	timer.f()
	require.Equal(t, CodeStopTimeout, runtimeTestRecord(t, ctx, r, RecordControl, "r.1").Code)
	retry := make(chan error, 1)
	go func() { retry <- r.Stop(ctx, "r.1") }()
	runtimeTestReceive(t, ctx, accepted)
	runtimeTestReceive(t, ctx, entered)
	require.Empty(t, stopped, "physical Stops overlapped")
	returns <- nil
	runtimeTestReceive(t, ctx, returned)
	runtimeTestCommitStopEntered(t, ctx, stopped, "queued Stop dispatch waited for retry commit")
	release <- struct{}{}
	require.NoError(t, runtimeTestReceive(t, ctx, retry))
	returns <- nil
	runtimeTestReceive(t, ctx, returned)
	a.report(Result{State: StateCancelled})
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}
