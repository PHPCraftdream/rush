package jobbatch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRuntimeCommitOutputOrdinaryErrorPreservesChunk(t *testing.T) {
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
			sentinel := errors.New("ordinary read error")
			expected := OutputChunk{Data: []byte("partial"), Next: 19, Oldest: 3, Truncated: true}
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{read: func(context.Context, uint64) (OutputChunk, error) { return expected, sentinel }})
			got, err := r.Output(ctx, "r.1", 3)
			require.ErrorIs(t, err, sentinel)
			require.Equal(t, expected, got, "ordinary executor error discarded partial output")
			runtimeTestFinish(t, ctx, r, a, StateCompleted)
			runtimeTestWait(t, ctx, b)
			runtimeTestClose(t, ctx, r)
		})
	}
}

func TestRuntimeCommitEventsGapFaultPriority(t *testing.T) {
	t.Parallel()
	for _, fault := range []bool{false, true} {
		name := "no_fault"
		if fault {
			name = "fault"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := runtimeTestContext(t)
			e := &runtimeTestExecutor{starts: make(chan *runtimeTestAttempt, 4)}
			r, err := NewRunner(context.Background(), RunnerConfig{Limits: Limits{MaxConcurrent: 1, MaxDepth: 8, MaxNodes: 32}, Executor: e, ControlTimeout: time.Second, RecordRetention: 1, CommitHook: func(_ context.Context, tr Transition) error {
				if fault && tr.Event.Type == EventCapacityAvailable {
					return ErrCommitFailed
				}
				return nil
			}, CommitTimeout: time.Second})
			require.NoError(t, err)
			b := runtimeTestSubmit(t, ctx, r, "r", runtimeTestBatch(ModeParallel, NotifyAll))
			runtimeTestWait(t, ctx, b)
			if fault {
				require.ErrorIs(t, r.CapacityAvailable(ctx, ""), ErrCommitFailed)
				runtimeTestReceive(t, ctx, r.Faulted())
			}
			expired, cancel := context.WithCancel(context.Background())
			cancel()
			for _, c := range []context.Context{ctx, expired} {
				page, err := r.Events(c, 1)
				if fault {
					require.ErrorIs(t, err, ErrCommitFailed, "retention gap masked recorded fault")
				} else {
					require.ErrorIs(t, err, ErrGap)
				}
				require.Empty(t, page.Records)
				require.Greater(t, page.Oldest, uint64(1))
			}
			if fault {
				require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
			} else {
				runtimeTestClose(t, ctx, r)
			}
		})
	}
}

func TestRuntimeCommitEventsNoFaultCancellationDoesNotDrain(t *testing.T) {
	t.Parallel()
	entered, resume := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	r, _, _, ctx := runtimeTestCommitHooks(t, func(context.Context, Transition) error { return nil }, &runnerRequestHooks{beforeEventsContext: func() {
		if armed.CompareAndSwap(true, false) {
			close(entered)
			<-resume
		}
	}})
	runtimeTestSubmit(t, ctx, r, "r", runtimeTestBatch(ModeParallel, NotifyAll))
	before := runtimeTestPage(t, ctx, r)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	armed.Store(true)
	reply := make(chan error, 1)
	go func() { _, err := r.Events(expired, before.Next); reply <- err }()
	runtimeTestReceive(t, ctx, entered)
	require.NoError(t, r.CapacityAvailable(ctx, ""))
	close(resume)
	require.ErrorIs(t, runtimeTestReceive(t, ctx, reply), context.Canceled, "no-fault cancelled wait drained newly appended records")
	runtimeTestClose(t, ctx, r)
}
