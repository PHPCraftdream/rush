package jobbatch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitOwnerFaultFences(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			entered, release := make(chan struct{}), make(chan struct{})
			accepted := make(chan *runnerRequest, 8)
			calls := make(chan EventType, 8)
			r, _, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
				calls <- tr.Event.Type
				if tr.Event.Type == EventCapacityAvailable {
					close(entered)
					<-release
					if mode == "panic" {
						panic("private")
					}
					return ErrCommitFailed
				}
				return nil
			}, &runnerRequestHooks{accepted: func(q *runnerRequest) { accepted <- q }})
			b := runtimeTestSubmit(t, ctx, r, "r", runtimeTestBatch(ModeParallel, NotifyAll))
			runtimeTestReceive(t, ctx, accepted)
			summary := runtimeTestWait(t, ctx, b)
			written := make(chan runnerAnswer, 1)
			// An already-written private reply must not be replaced by a later fault.
			q := &runnerRequest{event: Event{Type: EventStopRequested, Node: "r"}, reply: written}
			q.state.Store(runnerAccepted)
			r.reply(q, runnerAnswer{})
			capacity := make(chan error, 1)
			go func() { capacity <- r.CapacityAvailable(ctx, "") }()
			runtimeTestReceive(t, ctx, entered)
			runtimeTestReceive(t, ctx, accepted)
			rejected := make(chan error, 1)
			fenceCtx, cancelFence := context.WithCancel(ctx)
			go func() { rejected <- r.Stop(fenceCtx, "r") }()
			runtimeTestReceive(t, ctx, accepted)
			cancelFence()
			later := make(chan error, 1)
			go func() { later <- r.CapacityAvailable(ctx, "later") }()
			runtimeTestReceive(t, ctx, accepted)
			barrier := make(chan error, 1)
			go func() { barrier <- r.Stop(ctx, "unknown") }()
			runtimeTestReceive(t, ctx, accepted)
			select {
			case <-rejected:
				t.Fatal("recordless rejection replied before fence")
			default:
			}
			close(release)
			require.ErrorIs(t, runtimeTestReceive(t, ctx, capacity), ErrCommitFailed)
			require.ErrorIs(t, runtimeTestReceive(t, ctx, rejected), ErrCommitFailed, "fault fence unanswered")
			require.ErrorIs(t, runtimeTestReceive(t, ctx, later), ErrCommitFailed)
			require.ErrorIs(t, runtimeTestReceive(t, ctx, barrier), ErrCommitFailed)
			r.reply(q, runnerAnswer{err: ErrCommitFailed})
			require.NoError(t, runtimeTestReceive(t, ctx, written).err, "written reply replaced after fault")
			require.Equal(t, summary, runtimeTestWait(t, ctx, b))
			require.Len(t, calls, 2, "hook called after fault")
			page := runtimeTestPage(t, ctx, r)
			require.Zero(t, page.Final)
			_, err := r.Events(ctx, page.Next)
			require.ErrorIs(t, err, ErrCommitFailed)
			require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
		})
	}
}

func TestRuntimeCommitOwnerSpeculativeEligibility(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	accepted := make(chan *runnerRequest, 16)
	r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			close(entered)
			<-release
		}
		return nil
	}, &runnerRequestHooks{accepted: func(q *runnerRequest) { accepted <- q }})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	runtimeTestReceive(t, ctx, accepted)
	stopped := make(chan struct{}, 1)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return nil }})
	go func() { _ = r.CapacityAvailable(ctx, "") }()
	runtimeTestReceive(t, ctx, entered)
	runtimeTestReceive(t, ctx, accepted)
	stop := make(chan error, 1)
	go func() { stop <- r.Stop(ctx, "r") }()
	runtimeTestReceive(t, ctx, accepted)
	runtimeTestReceive(t, ctx, stopped)
	injection := make(chan error, 1)
	go func() { injection <- r.Inject(ctx, "r.1", nil) }()
	runtimeTestReceive(t, ctx, accepted)
	view := make(chan runnerAnswer, 1)
	go func() { view <- r.request(ctx, &runnerRequest{op: runnerRequestView, node: "r.1"}) }()
	runtimeTestReceive(t, ctx, accepted)
	close(release)
	require.NoError(t, runtimeTestReceive(t, ctx, stop))
	require.ErrorIs(t, runtimeTestReceive(t, ctx, injection), ErrTerminal)
	require.Equal(t, StateRunning, runtimeTestReceive(t, ctx, view).view.State, "observer read speculative cancellation")
	a.report(Result{State: StateCancelled})
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}
