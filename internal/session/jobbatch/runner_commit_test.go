package jobbatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func runtimeTestCommitNew(t *testing.T, hook func(context.Context, Transition) error) (*Runner, *runtimeTestExecutor, *runtimeTestClock, context.Context) {
	t.Helper()
	ctx := runtimeTestContext(t)
	e := &runtimeTestExecutor{starts: make(chan *runtimeTestAttempt, 32)}
	c := &runtimeTestClock{armed: make(chan *runtimeTestTimer, 128)}
	r, err := newRunnerWithClock(context.Background(), RunnerConfig{Limits: Limits{MaxConcurrent: 2, MaxDepth: 16, MaxNodes: 128}, Executor: e, ControlTimeout: 50 * time.Second, RecordRetention: 128, CommitHook: hook, CommitTimeout: time.Second}, c)
	require.NoError(t, err)
	return r, e, c, ctx
}

func TestRuntimeCommitSubmitBarrier(t *testing.T) {
	t.Parallel()
	entered := make(chan Transition, 8)
	release := make(chan struct{})
	r, e, _, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
		entered <- tr
		if tr.Event.Type == EventSubmit {
			<-release
		}
		return nil
	})
	reply := make(chan *Batch, 1)
	go func() {
		b, _ := r.Submit(ctx, SubmitRequest{Root: "r", Spec: OneItem(TaskSpec{Kind: "test"})})
		reply <- b
	}()
	runtimeTestReceive(t, ctx, entered)
	r.feed.mu.Lock()
	head := r.feed.head
	r.feed.mu.Unlock()
	require.Zero(t, head, "feed published before commit")
	select {
	case <-e.starts:
		t.Fatal("Start dispatched before commit")
	default:
	}
	select {
	case <-reply:
		t.Fatal("reply before commit")
	default:
	}
	close(release)
	b := runtimeTestReceive(t, ctx, reply)
	require.NotNil(t, b)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}

func TestRuntimeCommitFailure(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"error", "panic", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			held := make(chan struct{})
			entered := make(chan struct{}, 1)
			calls := make(chan EventType, 32)
			r, e, c, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
				calls <- tr.Event.Type
				if tr.Event.Type != EventSettled {
					return nil
				}
				entered <- struct{}{}
				switch mode {
				case "panic":
					panic("private")
				case "timeout":
					<-held
				}
				return errors.New("private")
			})
			b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
			before := runtimeTestPage(t, ctx, r)
			a.report(Result{State: StateCompleted})
			runtimeTestReceive(t, ctx, entered)
			if mode == "timeout" {
				// Submit, Started and Settled have each armed a commit timer.
				for range 3 {
					runtimeTestReceive(t, ctx, c.armed)
				}
				c.advance(time.Second)
			}
			runtimeTestReceive(t, ctx, r.Faulted())
			require.ErrorIs(t, r.FaultErr(), ErrCommitFailed)
			select {
			case <-b.Done():
				t.Fatal("uncommitted Done closed")
			default:
			}
			_, err := b.Wait(ctx)
			require.ErrorIs(t, err, ErrCommitFailed)
			page, err := r.Events(ctx, 1)
			require.NoError(t, err)
			require.Equal(t, before.Head, page.Head, "failed transition published")
			require.Zero(t, page.Final)
			_, err = r.Events(ctx, page.Next)
			require.ErrorIs(t, err, ErrCommitFailed)
			require.ErrorIs(t, r.Close(ctx), ErrCommitFailed, "fault Close must not succeed")
			require.ErrorIs(t, r.CapacityAvailable(ctx, ""), ErrCommitFailed)
			require.ErrorIs(t, r.Stop(ctx, "r"), ErrCommitFailed)
			require.ErrorIs(t, r.Release(ctx, "r"), ErrCommitFailed)
			_, err = r.View(ctx, "r")
			require.ErrorIs(t, err, ErrCommitFailed)
			_, err = r.Snapshot(ctx, "r")
			require.ErrorIs(t, err, ErrCommitFailed)
			_, err = r.Output(ctx, "r.1", 0)
			require.ErrorIs(t, err, ErrCommitFailed)
			require.ErrorIs(t, r.Inject(ctx, "r.1", nil), ErrCommitFailed)
			_, err = r.Add(ctx, AddRequest{Parent: "r", Items: []Spec{runtimeTestTask()}})
			require.ErrorIs(t, err, ErrCommitFailed)
			_, err = r.Submit(ctx, SubmitRequest{})
			require.ErrorIs(t, err, ErrCommitFailed)
			require.Len(t, calls, 3, "hook invoked after fault")
			close(held)
		})
	}
}

func TestRuntimeCommitHeldContainment(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	r, e, _, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventAdd {
			entered <- struct{}{}
			<-release
		}
		return nil
	})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	stopped := make(chan struct{}, 2)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return nil }})
	added := make(chan error, 1)
	go func() { _, err := r.Add(ctx, AddRequest{Parent: "r", Items: []Spec{runtimeTestTask()}}); added <- err }()
	runtimeTestReceive(t, ctx, entered)
	stopReply := make(chan error, 1)
	go func() { stopReply <- r.Stop(ctx, "r") }()
	runtimeTestReceive(t, ctx, stopped)
	select {
	case <-e.starts:
		t.Fatal("Add Start before commit")
	default:
	}
	select {
	case <-stopReply:
		t.Fatal("Stop reply before commit")
	default:
	}
	close(release)
	require.NoError(t, runtimeTestReceive(t, ctx, added))
	require.NoError(t, runtimeTestReceive(t, ctx, stopReply))
	// Add's committed Start can be in flight when Stop is accepted.
	select {
	case next := <-e.starts:
		require.Error(t, next.ctx.Err())
		next.decision <- runtimeTestDecision{err: errors.New("cancelled")}
	default:
	}
	a.report(Result{State: StateCancelled})
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}

func TestRuntimeCommitInFlightFault(t *testing.T) {
	t.Parallel()
	r, e, _, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			return ErrCommitFailed
		}
		return nil
	})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	a := runtimeTestReceive(t, ctx, e.starts)
	require.ErrorIs(t, r.CapacityAvailable(ctx, ""), ErrCommitFailed, "failed request got normal reply")
	runtimeTestReceive(t, ctx, r.Faulted())
	runtimeTestReceive(t, ctx, a.ctx.Done())
	stopped := make(chan struct{}, 2)
	disposed := make(chan struct{}, 1)
	a.decision <- runtimeTestDecision{execution: &runtimeTestExecution{stop: func(context.Context) error {
		stopped <- struct{}{}
		a.report(Result{State: StateCancelled})
		return nil
	}, dispose: func(context.Context) error { disposed <- struct{}{}; return nil }}}
	runtimeTestReceive(t, ctx, stopped)
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
	runtimeTestReceive(t, ctx, disposed)
	require.Empty(t, stopped, "containment Stop repeated")
	select {
	case <-b.Done():
		t.Fatal("uncommitted Done closed")
	default:
	}
}

func TestRuntimeCommitPublishedSummarySurvives(t *testing.T) {
	t.Parallel()
	r, _, _, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventHostClosing {
			return ErrCommitFailed
		}
		return nil
	})
	b := runtimeTestSubmit(t, ctx, r, "r", runtimeTestBatch(ModeParallel, NotifyAll))
	summary := runtimeTestWait(t, ctx, b)
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
	require.Equal(t, summary, runtimeTestWait(t, ctx, b))
	page := runtimeTestPage(t, ctx, r)
	require.Zero(t, page.Final)
	for _, rec := range page.Records {
		require.NotEqual(t, RecordHostClosing, rec.Type)
		require.NotEqual(t, RecordClosed, rec.Type)
	}
}

func TestRuntimeCommitTimeoutValidation(t *testing.T) {
	t.Parallel()
	cfg := RunnerConfig{Limits: Limits{MaxConcurrent: 1, MaxDepth: 2, MaxNodes: 8}, Executor: &runtimeTestExecutor{}, ControlTimeout: time.Second, RecordRetention: 8, CommitHook: func(context.Context, Transition) error { return nil }}
	for _, timeout := range []time.Duration{0, -time.Second} {
		cfg.CommitTimeout = timeout
		_, err := NewRunner(context.Background(), cfg)
		require.ErrorIs(t, err, ErrInvalidSpec)
	}
}
