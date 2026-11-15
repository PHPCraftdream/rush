package jobbatch

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func runtimeTestCommitHooks(t *testing.T, hook func(context.Context, Transition) error, hooks *runnerRequestHooks) (*Runner, *runtimeTestExecutor, *runtimeTestClock, context.Context) {
	t.Helper()
	ctx := runtimeTestContext(t)
	e := &runtimeTestExecutor{starts: make(chan *runtimeTestAttempt, 32)}
	c := &runtimeTestClock{armed: make(chan *runtimeTestTimer, 128)}
	r, err := newRunnerWithClockAndHooks(context.Background(), RunnerConfig{Limits: Limits{MaxConcurrent: 2, MaxDepth: 16, MaxNodes: 128}, Executor: e, ControlTimeout: 50 * time.Second, RecordRetention: 128, CommitHook: hook, CommitTimeout: time.Second}, c, hooks)
	require.NoError(t, err)
	return r, e, c, ctx
}

func TestRuntimeCommitTimeoutClaimsBeforeSuccess(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	queued, resume, returned := make(chan struct{}), make(chan struct{}), make(chan struct{}, 8)
	r, e, c, ctx := runtimeTestCommitHooks(t, func(context.Context, Transition) error {
		close(entered)
		<-release
		return nil
	}, &runnerRequestHooks{queued: func(q *runnerRequest) {
		if q.event.Type == EventStopRequested {
			close(queued)
			<-resume
		}
	}, commitReturned: func() { returned <- struct{}{} }})
	reply := make(chan error, 1)
	go func() {
		_, err := r.Submit(ctx, SubmitRequest{Root: "r", Spec: OneItem(TaskSpec{Kind: "test"})})
		reply <- err
	}()
	runtimeTestReceive(t, ctx, entered)
	runtimeTestReceive(t, ctx, c.armed)
	go func() { _ = r.Stop(ctx, "r") }()
	runtimeTestReceive(t, ctx, queued)
	// Keep the loop outside its select until both the timer and physical nil
	// return have occurred. Publication must follow the first claim, not select.
	c.advance(time.Second)
	close(release)
	runtimeTestReceive(t, ctx, returned)
	close(resume)
	require.ErrorIs(t, runtimeTestReceive(t, ctx, reply), ErrCommitFailed, "timeout claim lost to nil return")
	runtimeTestReceive(t, ctx, r.Faulted())
	page, err := r.Events(ctx, 1)
	require.ErrorIs(t, err, ErrCommitFailed)
	require.Zero(t, page.Head, "timed-out transition published")
	require.Zero(t, page.Final)
	require.Empty(t, e.starts, "timed-out Start dispatched")
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
}

func TestRuntimeCommitHeldSettledSerialSlowSuccess(t *testing.T) {
	t.Parallel()
	entered := make(chan Transition, 16)
	release := make(chan struct{})
	queued := make(chan struct{})
	r, e, c, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		entered <- tr
		if tr.Event.Type == EventSettled {
			<-release
		}
		return nil
	}, &runnerRequestHooks{queued: func(q *runnerRequest) {
		if q.event.Type == EventCapacityAvailable {
			close(queued)
		}
	}})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	require.Equal(t, EventSubmit, runtimeTestReceive(t, ctx, entered).Event.Type)
	require.Equal(t, EventStarted, runtimeTestReceive(t, ctx, entered).Event.Type)
	before := runtimeTestPage(t, ctx, r)
	a.report(Result{State: StateCompleted})
	settled := runtimeTestReceive(t, ctx, entered)
	require.Equal(t, EventSettled, settled.Event.Type)
	for range 3 {
		runtimeTestReceive(t, ctx, c.armed)
	}
	capacity := make(chan error, 1)
	// The loop observes the later request without entering its hook.
	go func() { capacity <- r.CapacityAvailable(ctx, "") }()
	runtimeTestReceive(t, ctx, queued)
	c.advance(500 * time.Millisecond)
	select {
	case <-b.Done():
		t.Fatal("held Settled closed Done")
	default:
	}
	require.Equal(t, before.Head, runtimeTestPage(t, ctx, r).Head, "held Settled published feed")
	require.Empty(t, entered, "hook overlapped held Settled")
	select {
	case <-capacity:
		t.Fatal("later request replied during held Settled")
	default:
	}
	close(release)
	runtimeTestWait(t, ctx, b)
	require.NoError(t, runtimeTestReceive(t, ctx, capacity))
	later := runtimeTestReceive(t, ctx, entered)
	require.Equal(t, EventCapacityAvailable, later.Event.Type)
	require.Equal(t, settled.Sequence+1, later.Sequence)
	require.NoError(t, r.FaultErr(), "slow success before timeout faulted")
	runtimeTestClose(t, ctx, r)
}

func TestRuntimeCommitAttemptTimerBarrier(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	r, e, c, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventSubmit {
			close(entered)
			<-release
		}
		return nil
	})
	reply := make(chan *Batch, 1)
	go func() {
		b, _ := r.Submit(ctx, SubmitRequest{Root: "r", Spec: OneItem(TaskSpec{Kind: "test", Timeout: 10 * time.Second})})
		reply <- b
	}()
	runtimeTestReceive(t, ctx, entered)
	runtimeTestReceive(t, ctx, c.armed) // Commit timer, not an attempt timer.
	require.Empty(t, c.armed, "attempt timer armed before commit")
	require.Empty(t, e.starts, "Start before commit")
	c.advance(500 * time.Millisecond)
	close(release)
	b := runtimeTestReceive(t, ctx, reply)
	timer := runtimeTestReceive(t, ctx, c.armed)
	require.Equal(t, 10500*time.Millisecond, timer.at, "attempt timeout began before committed dispatch")
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}

func TestRuntimeCommitAbandonedStopNoContainment(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	queued, resume, accepted := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventAdd {
			close(entered)
			<-release
		}
		return nil
	}, &runnerRequestHooks{queued: func(q *runnerRequest) {
		if q.event.Type == EventStopRequested {
			close(queued)
			<-resume
		}
	}, accepted: func(q *runnerRequest) {
		if q.event.Type == EventStopRequested {
			accepted <- struct{}{}
		}
	}})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	stopped := make(chan struct{}, 2)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return nil }})
	added := make(chan error, 1)
	go func() { _, err := r.Add(ctx, AddRequest{Parent: "r", Items: []Spec{runtimeTestTask()}}); added <- err }()
	runtimeTestReceive(t, ctx, entered)
	stopCtx, cancel := context.WithCancel(ctx)
	reply := make(chan error, 1)
	go func() { reply <- r.Stop(stopCtx, "r") }()
	runtimeTestReceive(t, ctx, queued)
	cancel()
	require.ErrorIs(t, runtimeTestReceive(t, ctx, reply), context.Canceled)
	close(resume)
	close(release)
	require.NoError(t, runtimeTestReceive(t, ctx, added))
	require.NoError(t, r.CapacityAvailable(ctx, "")) // Loop barrier after skipped Stop.
	require.Empty(t, accepted, "abandoned Stop accepted")
	require.Empty(t, stopped, "abandoned Stop dispatched physical cancellation")
	select {
	case <-a.ctx.Done():
		t.Fatal("abandoned Stop cancelled Start context")
	default:
	}
	for _, rec := range runtimeTestPage(t, ctx, r).Records {
		require.NotEqual(t, RecordStopRequested, rec.Type)
	}
	next := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestFinish(t, ctx, r, next, StateCompleted)
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}
