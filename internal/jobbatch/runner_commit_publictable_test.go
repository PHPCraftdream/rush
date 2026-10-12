package jobbatch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitAllPublicPostFault(t *testing.T) {
	t.Parallel()
	for _, exited := range []bool{false, true} {
		name := "draining"
		if exited {
			name = "exited"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, e, _, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
				if tr.Event.Type == EventCapacityAvailable {
					return ErrCommitFailed
				}
				return nil
			})
			cached := runtimeTestSubmit(t, ctx, r, "cached", runtimeTestBatch(ModeParallel, NotifyAll))
			summary := runtimeTestWait(t, ctx, cached)
			live := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
			require.ErrorIs(t, r.CapacityAvailable(ctx, ""), ErrCommitFailed)
			runtimeTestReceive(t, ctx, r.Faulted())
			if exited {
				a.report(Result{State: StateCancelled})
				require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
				runtimeTestReceive(t, ctx, r.exited)
			}
			expired, cancel := context.WithCancel(context.Background())
			cancel()
			calls := []struct {
				name string
				call func(context.Context) error
			}{
				{"Submit", func(c context.Context) error {
					_, err := r.Submit(c, SubmitRequest{Root: "other", Spec: runtimeTestBatch(ModeParallel, NotifyAll)})
					return err
				}},
				{"Add", func(c context.Context) error {
					_, err := r.Add(c, AddRequest{Parent: "r", Items: []Spec{runtimeTestTask()}})
					return err
				}},
				{"Stop", func(c context.Context) error { return r.Stop(c, "r") }},
				{"Release", func(c context.Context) error { return r.Release(c, "r") }},
				{"Capacity", func(c context.Context) error { return r.CapacityAvailable(c, "") }},
				{"Close", r.Close},
				{"View", func(c context.Context) error { _, err := r.View(c, "r"); return err }},
				{"Snapshot", func(c context.Context) error { _, err := r.Snapshot(c, "r"); return err }},
				{"Output", func(c context.Context) error { _, err := r.Output(c, "r.1", 0); return err }},
				{"Inject", func(c context.Context) error { return r.Inject(c, "r.1", nil) }},
				{"Wait", func(c context.Context) error { _, err := live.Wait(c); return err }},
			}
			for _, c := range []context.Context{ctx, expired} {
				for _, call := range calls {
					// Close is bounded while live work has not reported termination.
					callCtx := c
					if call.name == "Close" && !exited {
						callCtx = expired
					}
					require.ErrorIs(t, call.call(callCtx), ErrCommitFailed, call.name)
				}
				got, err := cached.Wait(c)
				require.NoError(t, err)
				require.Equal(t, summary, got)
				page, err := r.Events(c, 1)
				require.NoError(t, err)
				require.NotEmpty(t, page.Records)
				_, err = r.Events(c, page.Next)
				require.ErrorIs(t, err, ErrCommitFailed)
			}
			require.Equal(t, ID("r"), live.ID())
			require.ErrorIs(t, r.FaultErr(), ErrCommitFailed)
			select {
			case <-live.Done():
				t.Fatal("fault closed unpublished Done")
			default:
			}
			if !exited {
				a.report(Result{State: StateCancelled})
				require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
			}
		})
	}
}

func TestRuntimeCommitPriorityNoFaultControls(t *testing.T) {
	t.Parallel()
	r, _, _, ctx := runtimeTestCommitNew(t, func(context.Context, Transition) error { return nil })
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.View(expired, "missing")
	require.ErrorIs(t, err, context.Canceled)
	_, err = r.Events(expired, 1)
	require.ErrorIs(t, err, context.Canceled)
	b := runtimeTestSubmit(t, ctx, r, "r", runtimeTestBatch(ModeParallel, NotifyAll))
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
	require.ErrorIs(t, r.Stop(ctx, "r"), ErrClosed)
	_, err = r.View(ctx, "r")
	require.NoError(t, err)
	_, err = r.Snapshot(ctx, "r")
	require.NoError(t, err)
}
