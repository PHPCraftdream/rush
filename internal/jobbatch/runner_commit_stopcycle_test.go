package jobbatch

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func runtimeTestCommitStopEntered(t *testing.T, ctx context.Context, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(message)
	}
}

func TestRuntimeCommitInflightAuthorizedStop(t *testing.T) {
	t.Parallel()
	for _, timeout := range []bool{false, true} {
		name := "success"
		if timeout {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entered, release := make(chan struct{}), make(chan struct{})
			r, e, c, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
				if tr.Event.Type == EventStarted {
					close(entered)
					<-release
				}
				return nil
			})
			defer close(release)
			b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
			a := runtimeTestReceive(t, ctx, e.starts)
			require.NoError(t, r.Stop(ctx, "r.1"))
			runtimeTestReceive(t, ctx, a.ctx.Done())
			stopped := make(chan struct{}, 2)
			a.decision <- runtimeTestDecision{execution: &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return nil }}}
			runtimeTestReceive(t, ctx, entered)
			runtimeTestCommitStopEntered(t, ctx, stopped, "authorized inflight Stop waited for Started commit")
			require.Empty(t, stopped)
			if timeout {
				c.advance(time.Second)
				runtimeTestReceive(t, ctx, r.Faulted())
				a.report(Result{State: StateCancelled})
				require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
			} else {
				// Release without closing twice in the cleanup.
				release <- struct{}{}
				runtimeTestRecord(t, ctx, r, RecordStarted, "r.1")
				a.report(Result{State: StateCancelled})
				runtimeTestWait(t, ctx, b)
				runtimeTestClose(t, ctx, r)
			}
			require.Empty(t, stopped, "authorized Stop duplicated")
		})
	}
}

func TestRuntimeCommitRetryCycle(t *testing.T) {
	t.Parallel()
	for _, held := range []EventType{EventCapacityAvailable, EventStopRequested} {
		t.Run(string(held), func(t *testing.T) {
			t.Parallel()
			entered, release := make(chan struct{}), make(chan struct{})
			accepted := make(chan struct{}, 2)
			returns := make(chan struct{}, 4)
			stopEvents := 0
			r, e, _, ctx := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
				if tr.Event.Type == EventStopRequested {
					stopEvents++
				}
				if (held == EventCapacityAvailable && tr.Event.Type == held) || (held == EventStopRequested && tr.Event.Type == held && stopEvents == 2) {
					close(entered)
					<-release
				}
				return nil
			}, &runnerRequestHooks{accepted: func(q *runnerRequest) {
				if q.event.Type == EventStopRequested {
					accepted <- struct{}{}
				}
			}, controlReturned: func() { returns <- struct{}{} }})
			b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
			stopped := make(chan struct{}, 4)
			decisions := make(chan error, 4)
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return <-decisions }})
			require.NoError(t, r.Stop(ctx, "r.1"))
			runtimeTestReceive(t, ctx, accepted)
			runtimeTestReceive(t, ctx, stopped)
			decisions <- ErrUnsupported
			runtimeTestReceive(t, ctx, returns)
			require.Equal(t, CodeStopFailed, runtimeTestRecord(t, ctx, r, RecordControl, "r.1").Code)
			before := runtimeTestPage(t, ctx, r)
			capacity := make(chan error, 1)
			if held == EventCapacityAvailable {
				go func() { capacity <- r.CapacityAvailable(ctx, "") }()
				runtimeTestReceive(t, ctx, entered)
			}
			retry := make(chan error, 1)
			go func() { retry <- r.Stop(ctx, "r.1") }()
			runtimeTestReceive(t, ctx, accepted)
			if held == EventCapacityAvailable {
				close(release)
				require.NoError(t, runtimeTestReceive(t, ctx, capacity))
			} else {
				runtimeTestReceive(t, ctx, entered)
			}
			runtimeTestCommitStopEntered(t, ctx, stopped, "authorized retry Stop dropped or waited for its commit")
			if held == EventStopRequested {
				require.Equal(t, before.Head, runtimeTestPage(t, ctx, r).Head)
				close(release)
			}
			require.NoError(t, runtimeTestReceive(t, ctx, retry))
			decisions <- nil
			runtimeTestReceive(t, ctx, returns)
			cursor := before.Next
			found := false
			for !found {
				page, err := r.Events(ctx, cursor)
				require.NoError(t, err)
				for _, rec := range page.Records {
					if rec.Type == RecordControl {
						require.Empty(t, rec.Code)
						found = true
					}
				}
				cursor = page.Next
			}
			require.Empty(t, stopped, "retry physical Stop duplicated")
			a.report(Result{State: StateCancelled})
			runtimeTestWait(t, ctx, b)
			runtimeTestClose(t, ctx, r)
		})
	}
}
