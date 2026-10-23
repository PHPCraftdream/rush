package jobbatch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type runtimeTestCompletionOnlyExecution struct{}

func (*runtimeTestCompletionOnlyExecution) Stop(context.Context) error { return nil }

func TestRuntimeCommitConsumedCompletionFaultBoundary(t *testing.T) {
	t.Parallel()
	for _, disposer := range []bool{false, true} {
		name := "non_disposer"
		if disposer {
			name = "disposer"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hookEntered, hookRelease := make(chan struct{}), make(chan struct{})
			completionEntered, completionResume := make(chan struct{}), make(chan struct{})
			processed, ownerResume := make(chan struct{}), make(chan struct{})
			disposed := make(chan struct{}, 2)
			r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
				if tr.Event.Type == EventCapacityAvailable {
					close(hookEntered)
					<-hookRelease
					return ErrCommitFailed
				}
				return nil
			}, &runnerRequestHooks{beforeSettlementApply: func() { close(completionEntered); <-completionResume }, afterMessage: func(m runnerMessage) {
				if m.kind == runnerCompletion {
					close(processed)
					<-ownerResume
				}
			}})
			b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
			var execution Execution = &runtimeTestCompletionOnlyExecution{}
			if disposer {
				execution = &runtimeTestExecution{dispose: func(context.Context) error { disposed <- struct{}{}; return nil }}
			}
			a := runtimeTestAccept(t, ctx, r, e, execution)
			before := runtimeTestPage(t, ctx, r)
			go func() { _ = r.CapacityAvailable(ctx, "") }()
			runtimeTestReceive(t, ctx, hookEntered)
			a.report(Result{State: StateCompleted}) // Exactly one authoritative report.
			runtimeTestReceive(t, ctx, completionEntered)
			close(hookRelease)
			runtimeTestReceive(t, ctx, r.Faulted())
			close(completionResume)
			runtimeTestReceive(t, ctx, processed)
			defer close(ownerResume)
			r.mu.Lock()
			h := r.handles[0]
			r.mu.Unlock()
			h.mu.Lock()
			terminated := h.terminated
			h.mu.Unlock()
			require.True(t, terminated, "consumed report lost termination across failed Apply")
			ownerResume <- struct{}{}
			runtimeTestCommitStopEntered(t, ctx, r.exited, "owner did not drain consumed completion")
			require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
			if disposer {
				runtimeTestReceive(t, ctx, disposed)
				require.Empty(t, disposed, "Dispose repeated")
			}
			require.Equal(t, before.Head, runtimeTestPage(t, ctx, r).Head)
			select {
			case <-b.Done():
				t.Fatal("uncommitted completion published Done")
			default:
			}
		})
	}
}

func TestRuntimeCommitBufferedReportStartedFault(t *testing.T) {
	t.Parallel()
	hookEntered, hookRelease := make(chan struct{}), make(chan struct{})
	reportSeen := make(chan struct{}, 1)
	startedEntered, startedResume := make(chan struct{}), make(chan struct{})
	r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable {
			close(hookEntered)
			<-hookRelease
			return ErrCommitFailed
		}
		return nil
	}, &runnerRequestHooks{afterMessage: func(m runnerMessage) {
		if m.kind == runnerCompletion {
			reportSeen <- struct{}{}
		}
	}, beforeStartedApply: func() { close(startedEntered); <-startedResume }})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	a := runtimeTestReceive(t, ctx, e.starts)
	go func() { _ = r.CapacityAvailable(ctx, "") }()
	runtimeTestReceive(t, ctx, hookEntered)
	a.report(Result{State: StateCompleted})
	runtimeTestReceive(t, ctx, reportSeen)
	disposed := make(chan struct{}, 2)
	a.decision <- runtimeTestDecision{execution: &runtimeTestExecution{dispose: func(context.Context) error { disposed <- struct{}{}; return nil }}}
	runtimeTestReceive(t, ctx, startedEntered)
	close(hookRelease)
	runtimeTestReceive(t, ctx, r.Faulted())
	close(startedResume)
	runtimeTestReceive(t, ctx, disposed)
	runtimeTestCommitStopEntered(t, ctx, r.exited, "buffered completion did not drain after Started Apply fault")
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
	require.Empty(t, disposed)
	select {
	case <-b.Done():
		t.Fatal("buffered fault published Done")
	default:
	}
}
