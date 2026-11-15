package jobbatch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitEventBarriers(t *testing.T) {
	t.Parallel()
	for _, typ := range []EventType{EventStopRequested, EventRelease, EventCapacityAvailable, EventHostClosing} {
		t.Run(string(typ), func(t *testing.T) {
			t.Parallel()
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			r, e, _, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
				if tr.Event.Type == typ {
					entered <- struct{}{}
					<-release
				}
				return nil
			})
			b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
			stopped := make(chan struct{}, 2)
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return nil }})
			if typ == EventRelease {
				runtimeTestFinish(t, ctx, r, a, StateCompleted)
				runtimeTestWait(t, ctx, b)
			}
			before := runtimeTestPage(t, ctx, r)
			reply := make(chan error, 1)
			go func() {
				switch typ {
				case EventStopRequested:
					reply <- r.Stop(ctx, "r")
				case EventRelease:
					reply <- r.Release(ctx, "r")
				case EventCapacityAvailable:
					reply <- r.CapacityAvailable(ctx, "")
				case EventHostClosing:
					reply <- r.Close(ctx)
				}
			}()
			runtimeTestReceive(t, ctx, entered)
			page := runtimeTestPage(t, ctx, r)
			require.Equal(t, before.Head, page.Head, "event published before commit")
			require.Zero(t, page.Final)
			select {
			case <-reply:
				t.Fatal("reply before commit")
			default:
			}
			if typ == EventStopRequested || typ == EventHostClosing {
				runtimeTestReceive(t, ctx, stopped)
			}
			close(release)
			if typ != EventRelease {
				a.report(Result{State: StateCompleted})
				runtimeTestWait(t, ctx, b)
			}
			require.NoError(t, runtimeTestReceive(t, ctx, reply))
			if typ != EventHostClosing {
				runtimeTestClose(t, ctx, r)
			}
		})
	}
}

func TestRuntimeCommitEmptyDoneBarrier(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var batch *Batch
	r, _, _, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventSubmit {
			entered <- struct{}{}
			<-release
		}
		return nil
	})
	reply := make(chan *Batch, 1)
	go func() {
		b, _ := r.Submit(ctx, SubmitRequest{Root: "r", Spec: runtimeTestBatch(ModeParallel, NotifyAll)})
		reply <- b
	}()
	runtimeTestReceive(t, ctx, entered)
	// Only the loop writes this map; the hook-entry channel is the read barrier.
	batch = r.batches["r"]
	select {
	case <-batch.Done():
		t.Fatal("Done before commit")
	default:
	}
	close(release)
	require.Equal(t, batch, runtimeTestReceive(t, ctx, reply))
	runtimeTestWait(t, ctx, batch)
	runtimeTestClose(t, ctx, r)
}

func TestRuntimeCommitFaultCloseBounded(t *testing.T) {
	t.Parallel()
	r, e, _, ctx := runtimeTestCommitNew(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventAdd {
			return ErrCommitFailed
		}
		return nil
	})
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test"}))
	stopped := make(chan struct{}, 2)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { stopped <- struct{}{}; return ErrUnsupported }})
	_, err := r.Add(ctx, AddRequest{Parent: "r", Items: []Spec{runtimeTestTask()}})
	require.ErrorIs(t, err, ErrCommitFailed)
	runtimeTestReceive(t, ctx, stopped)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err = r.Close(cancelled)
	require.ErrorIs(t, err, ErrCommitFailed)
	require.NotErrorIs(t, err, ErrCloseTimeout)
	select {
	case <-b.Done():
		t.Fatal("fault closed live Done")
	default:
	}
	a.report(Result{State: StateCancelled})
	require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
	require.Empty(t, stopped, "failed containment control retried")
	select {
	case <-e.starts:
		t.Fatal("failed Add dispatched Start")
	default:
	}
}
