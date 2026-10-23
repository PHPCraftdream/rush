package jobbatch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitFaultReturnBeforeOwnerCleanup(t *testing.T) {
	t.Parallel()
	hookEntered, hookRelease := make(chan struct{}), make(chan struct{})
	ownerEntered, ownerResume := make(chan struct{}), make(chan struct{})
	registered, ownerCleanup := make(chan struct{}), make(chan struct{})
	r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			close(hookEntered)
			<-hookRelease
			return ErrCommitFailed
		}
		return nil
	}, &runnerRequestHooks{beforeMessage: func(m runnerMessage) {
		if m.kind == runnerStartReturned {
			close(ownerEntered)
			<-ownerResume
		}
	}, afterMessage: func(m runnerMessage) {
		if m.kind == runnerStartReturned {
			close(registered)
			<-ownerCleanup
		}
	}})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	a := runtimeTestReceive(t, ctx, e.starts)
	go func() { _ = r.CapacityAvailable(ctx, "") }()
	runtimeTestReceive(t, ctx, hookEntered)
	stopped := make(chan struct{}, 2)
	a.decision <- runtimeTestDecision{execution: &runtimeTestExecution{stop: func(context.Context) error {
		stopped <- struct{}{}
		a.report(Result{State: StateCancelled})
		return nil
	}}}
	runtimeTestReceive(t, ctx, ownerEntered)
	close(hookRelease)
	runtimeTestReceive(t, ctx, r.Faulted())
	close(ownerResume)
	runtimeTestReceive(t, ctx, registered)
	defer close(ownerCleanup)
	runtimeTestCommitStopEntered(t, ctx, stopped, "fault-return handle given up before owed Stop")
	runtimeTestReceive(t, ctx, a.ctx.Done())
	require.Empty(t, stopped, "fault return Stop duplicated")
	// Allow the owner to finish containment, then bounded Close must drain.
	ownerCleanup <- struct{}{}
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
	select {
	case <-b.Done():
		t.Fatal("fault published Done")
	default:
	}
}

type runtimeTestImmediateCommitClock struct {
	mu     sync.Mutex
	timers []*runtimeTestImmediateTimer
}
type runtimeTestImmediateTimer struct{}

func (*runtimeTestImmediateTimer) Stop() bool { return false }
func (c *runtimeTestImmediateCommitClock) AfterFunc(_ time.Duration, callback func()) runnerTimer {
	timer := &runtimeTestImmediateTimer{}
	c.mu.Lock()
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	callback()
	return timer
}

func TestRuntimeCommitTimeoutBeforeHookStart(t *testing.T) {
	t.Parallel()
	ctx := runtimeTestContext(t)
	calls := make(chan struct{}, 2)
	physicalReturned := make(chan struct{}, 1)
	e := &runtimeTestExecutor{starts: make(chan *runtimeTestAttempt, 4)}
	r, err := newRunnerWithClockAndHooks(context.Background(), RunnerConfig{Limits: Limits{MaxConcurrent: 1, MaxDepth: 8, MaxNodes: 32}, Executor: e, ControlTimeout: time.Second, RecordRetention: 32, CommitHook: func(context.Context, Transition) error { calls <- struct{}{}; return nil }, CommitTimeout: time.Second}, &runtimeTestImmediateCommitClock{}, &runnerRequestHooks{commitReturned: func() { physicalReturned <- struct{}{} }})
	require.NoError(t, err)
	_, err = r.Submit(ctx, SubmitRequest{Root: "r", Spec: OneItem(TaskSpec{Kind: "test"})})
	require.ErrorIs(t, err, ErrCommitFailed)
	runtimeTestReceive(t, ctx, r.Faulted())
	runtimeTestReceive(t, ctx, physicalReturned)
	require.Empty(t, calls, "hook entered after timeout had claimed fault")
	require.Empty(t, e.starts)
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
}
