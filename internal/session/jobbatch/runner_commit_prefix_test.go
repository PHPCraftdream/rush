package jobbatch

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type runtimeTestPrefixWaitingContext struct {
	context.Context
	checked chan struct{}
	first   atomic.Bool
}

func (c *runtimeTestPrefixWaitingContext) Err() error {
	if c.first.CompareAndSwap(false, true) {
		close(c.checked)
		return nil
	}
	return c.Context.Err()
}

type runtimeTestPrefixAnswer struct {
	page EventPage
	err  error
}

func runtimeTestEventsPrefixBoundary(t *testing.T, waiting, fault bool) {
	t.Helper()
	ctx := runtimeTestContext(t)
	beforeContext, enterContext := make(chan struct{}), make(chan struct{})
	beforeFault, arbitrate := make(chan struct{}), make(chan struct{})
	failedHook, fail := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	r, _, _, _ := runtimeTestCommitHooks(t, func(_ context.Context, tr Transition) error {
		if tr.Event.Type == EventCapacityAvailable && tr.Event.Group == "failure" {
			close(failedHook)
			<-fail
			return ErrCommitFailed
		}
		return nil
	}, &runnerRequestHooks{
		beforeEventsContext: func() {
			if armed.Load() {
				close(beforeContext)
				<-enterContext
			}
		},
		beforeEventsFault: func() {
			if armed.CompareAndSwap(true, false) {
				close(beforeFault)
				<-arbitrate
			}
		},
	})
	runtimeTestSubmit(t, ctx, r, "r", runtimeTestBatch(ModeParallel, NotifyAll))
	before := runtimeTestPage(t, ctx, r)
	eventCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !waiting {
		cancel()
	}
	observed := eventCtx
	var checked chan struct{}
	if waiting {
		checked = make(chan struct{})
		observed = &runtimeTestPrefixWaitingContext{Context: eventCtx, checked: checked}
	}
	armed.Store(true)
	answer := make(chan runtimeTestPrefixAnswer, 1)
	go func() { page, err := r.Events(observed, before.Next); answer <- runtimeTestPrefixAnswer{page, err} }()
	runtimeTestReceive(t, ctx, beforeContext)
	close(enterContext)
	if waiting {
		runtimeTestReceive(t, ctx, checked)
		cancel()
	}
	// Waiting mode's first Err read returns nil before cancellation, so the
	// new seam can only be reached through the select's ctx.Done arm.
	runtimeTestReceive(t, ctx, beforeFault)
	require.NoError(t, r.CapacityAvailable(ctx, "prefix"))
	published, err := r.Events(ctx, before.Next)
	require.NoError(t, err)
	require.Greater(t, published.Head, before.Head, "prefix did not publish while Events was paused")
	require.Equal(t, "prefix", published.Records[0].Group)
	if fault {
		failed := make(chan error, 1)
		go func() { failed <- r.CapacityAvailable(ctx, "failure") }()
		runtimeTestReceive(t, ctx, failedHook)
		close(fail)
		runtimeTestReceive(t, ctx, r.Faulted())
		require.ErrorIs(t, runtimeTestReceive(t, ctx, failed), ErrCommitFailed)
	}
	// Both successful publication and (when requested) the subsequent fault
	// have occurred before letting the cancelled Events call arbitrate.
	close(arbitrate)
	got := runtimeTestReceive(t, ctx, answer)
	if fault {
		require.NoError(t, got.err, "cancelled Events fault hid newly committed prefix")
		require.Equal(t, published.Records, got.page.Records)
		require.Equal(t, published.Next, got.page.Next)
		require.Zero(t, got.page.Final)
		tail, err := r.Events(ctx, got.page.Next)
		require.ErrorIs(t, err, ErrCommitFailed)
		require.Empty(t, tail.Records)
		require.Zero(t, tail.Final)
		require.ErrorIs(t, r.Close(ctx), ErrCommitFailed)
	} else {
		require.ErrorIs(t, got.err, context.Canceled)
		require.Empty(t, got.page.Records)
		require.Equal(t, before.Head, got.page.Head, "no-fault cancellation changed stale page")
		require.Equal(t, before.Next, got.page.Next)
		runtimeTestClose(t, ctx, r)
	}
}

func TestRuntimeCommitCancelledPrefixBeforeFault(t *testing.T) {
	t.Parallel()
	for _, waiting := range []bool{false, true} {
		name := "pre_cancelled"
		if waiting {
			name = "waiting_select"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for range 200 {
				runtimeTestEventsPrefixBoundary(t, waiting, true)
			}
		})
	}
}

func TestRuntimeCommitCancelledPrefixNoFaultControl(t *testing.T) {
	t.Parallel()
	for _, waiting := range []bool{false, true} {
		name := "pre_cancelled"
		if waiting {
			name = "waiting_select"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for range 200 {
				runtimeTestEventsPrefixBoundary(t, waiting, false)
			}
		})
	}
}
