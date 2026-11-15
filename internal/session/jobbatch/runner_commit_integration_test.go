package jobbatch

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitReplyBeforeStart(t *testing.T) {
	t.Parallel()
	ctx := runtimeTestContext(t)
	e := &runtimeTestExecutor{starts: make(chan *runtimeTestAttempt, 8)}
	c := &runtimeTestClock{armed: make(chan *runtimeTestTimer, 32)}
	r, err := newRunnerWithClockAndHooks(context.Background(), RunnerConfig{Limits: Limits{MaxConcurrent: 1, MaxDepth: 8, MaxNodes: 32}, Executor: e, ControlTimeout: time.Second, RecordRetention: 32, CommitHook: func(context.Context, Transition) error { return nil }, CommitTimeout: time.Second}, c, &runnerRequestHooks{reply: func(q *runnerRequest) {
		if q.event.Type == EventSubmit {
			require.Empty(t, e.starts, "Start before reply fence")
			c.mu.Lock()
			defer c.mu.Unlock()
			for _, timer := range c.timers {
				require.NotEqual(t, 10*time.Second, timer.at, "attempt timer before reply fence")
			}
		}
	}})
	require.NoError(t, err)
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test", Timeout: 10 * time.Second}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}

func TestRuntimeCommitSnapshotNilDurable(t *testing.T) {
	t.Parallel()
	for _, durable := range []bool{false, true} {
		name := "nil"
		if durable {
			name = "durable"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var r *Runner
			var e *runtimeTestExecutor
			var ctx context.Context
			if durable {
				r, e, _, ctx = runtimeTestCommitNew(t, func(context.Context, Transition) error { return nil })
			} else {
				r, e, _, ctx = runtimeTestNew(t, 1, 128, nil)
			}
			b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
			snapshot, err := r.Snapshot(ctx, "r")
			require.NoError(t, err)
			require.Equal(t, StateRunning, snapshot.Descendants[0].State, "root snapshot stale after Started")
			started := runtimeTestRecord(t, ctx, r, RecordStarted, "r.1")
			require.Equal(t, started.Sequence, snapshot.Sequence)
			require.NoError(t, r.Stop(ctx, "r.1"))
			control := runtimeTestRecord(t, ctx, r, RecordControl, "r.1")
			snapshot, err = r.Snapshot(ctx, "r")
			require.NoError(t, err)
			require.Equal(t, StateCancelling, snapshot.Descendants[0].State)
			require.Equal(t, CauseStop, snapshot.Descendants[0].Cause)
			require.False(t, snapshot.Descendants[0].ControlPending)
			require.Equal(t, control.Sequence, snapshot.Sequence)
			a.report(Result{State: StateCancelled})
			runtimeTestWait(t, ctx, b)
			runtimeTestClose(t, ctx, r)
			snapshot, err = r.Snapshot(ctx, "r")
			require.NoError(t, err)
			closed := runtimeTestRecord(t, ctx, r, RecordClosed, "")
			require.Equal(t, closed.Sequence, snapshot.Sequence)
		})
	}
}

func TestRuntimeCommitOutputHeldStarted(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	accepted := make(chan struct{}, 1)
	r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventStarted {
			close(entered)
			<-release
		}
		return nil
	}, &runnerRequestHooks{accepted: func(q *runnerRequest) {
		if q.op == runnerRequestOutput {
			accepted <- struct{}{}
		}
	}})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	a := runtimeTestReceive(t, ctx, e.starts)
	a.decision <- runtimeTestDecision{execution: &runtimeTestExecution{}}
	runtimeTestReceive(t, ctx, entered)
	answer := make(chan error, 1)
	go func() { _, err := r.Output(ctx, "r.1", 0); answer <- err }()
	runtimeTestReceive(t, ctx, accepted)
	close(release)
	require.ErrorIs(t, runtimeTestReceive(t, ctx, answer), ErrNotStarted, "Output observed unpublished Started handle")
	a.report(Result{State: StateCompleted})
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}
