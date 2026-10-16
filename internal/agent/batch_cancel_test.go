package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// stopCapture records the context Stop received PLUS its Err() AT CAPTURE
// TIME: the 30s control timeout could make Err non-nil if asserted later, so
// the receiving goroutine must read the captured value.
type stopCapture struct {
	ctx    context.Context
	ctxErr error
}

// fakeCancelRunner records the context Stop was submitted under and can block
// until released (signals, never sleeps).
type fakeCancelRunner struct {
	gotCtx   chan stopCapture
	stopRoot chan jobbatch.ID
	release  chan struct{}
	stopErr  error
	stops    atomic.Int64
	// doneRoot is the root's completion signal; it stays OPEN in every test
	// (held termination) and cancel must never wait on it.
	doneRoot chan struct{}
	awaits   chan struct{}
}

func newFakeCancelRunner() *fakeCancelRunner {
	return &fakeCancelRunner{
		gotCtx:   make(chan stopCapture, 1),
		stopRoot: make(chan jobbatch.ID, 1),
		release:  make(chan struct{}),
		doneRoot: make(chan struct{}),
		awaits:   make(chan struct{}, 1),
	}
}

func (f *fakeCancelRunner) Submit(_ context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, error) {
	return req.Root, nil
}

func (f *fakeCancelRunner) Stop(ctx context.Context, id jobbatch.ID) error {
	f.stops.Add(1)
	f.gotCtx <- stopCapture{ctx: ctx, ctxErr: ctx.Err()} // Err frozen now
	f.stopRoot <- id
	<-f.release
	return f.stopErr
}

// fakeStopOnlyRunner satisfies batchRunnerHandle for tests that never submit.
type fakeStopOnlyRunner struct{ f *fakeCancelRunner }

func (s fakeStopOnlyRunner) Submit(context.Context, jobbatch.SubmitRequest) (jobbatch.ID, error) {
	return "", errors.New("not used")
}

func (s fakeStopOnlyRunner) Stop(ctx context.Context, id jobbatch.ID) error { return s.f.Stop(ctx, id) }

// Acceptance: sync caller cancellation submits Runner.Stop with a control
// context that is NOT the caller's cancelled context and carries its own
// timeout; the outcome is reported promptly.
func TestBatchCancelControlContextSurvivesCallerCancel(t *testing.T) {
	t.Parallel()
	runner := newFakeCancelRunner()
	caller, cancel := context.WithCancel(context.Background())
	cancel() // The caller is already gone when the Stop must still land.
	go func() {
		cap := <-runner.gotCtx
		require.NoError(t, cap.ctxErr, "control context must NOT be the cancelled caller ctx")
		_, hasDeadline := cap.ctx.Deadline()
		require.True(t, hasDeadline, "control context must carry its own bound")
		close(runner.release)
	}()
	out := cancelBatchCall(caller, fakeStopOnlyRunner{runner}, "root-1")
	require.NoError(t, out.Err)
	require.True(t, out.Submitted)
	require.Equal(t, jobbatch.ID("root-1"), <-runner.stopRoot)
	require.False(t, out.Settled, "a Stop acknowledgement never settles")
}

// Acceptance: cancellation with held termination. The caller returns promptly
// while the root's real report is still pending; cancel never waits for the
// root's Done and never settles the admission unit.
func TestBatchCancelHeldTerminationDoesNotSettle(t *testing.T) {
	t.Parallel()
	runner := newFakeCancelRunner()
	out := make(chan batchCancelOutcome, 1)
	go func() { out <- cancelBatchCall(context.Background(), fakeStopOnlyRunner{runner}, "root-1") }()
	<-runner.gotCtx // Stop submitted.
	// The root keeps running: doneRoot stays open. The caller must still get
	// its outcome only after Stop returns, and the outcome must not claim
	// settlement.
	close(runner.release)
	select {
	case o := <-out:
		require.False(t, o.Settled, "held termination keeps the admission unit occupied")
		require.True(t, o.Submitted)
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation never returned while the root was held")
	}
	select {
	case <-runner.doneRoot:
		t.Fatal("the root must still be running (held termination)")
	default:
	}
}

// A Stop submission failure is reported, still without settling.
func TestBatchCancelStopErrorReported(t *testing.T) {
	t.Parallel()
	runner := newFakeCancelRunner()
	runner.stopErr = errors.New("engine refused")
	go func() {
		<-runner.gotCtx
		close(runner.release)
	}()
	out := cancelBatchCall(context.Background(), fakeStopOnlyRunner{runner}, "root-1")
	require.Error(t, out.Err)
	require.False(t, out.Submitted)
	require.False(t, out.Settled)
}

// A nil runner fails closed with a typed outcome rather than panicking.
func TestBatchCancelNilRunner(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { cancelBatchCall(context.Background(), nil, "root-1") },
		"nil runner is a programming error; R11F wires the handle")
}

var errStopRefused = errors.New("engine refused")

// cancelDeliveryImpl builds a delivery whose Runner is the given cancel fake.
func cancelDeliveryImpl(done *fakeDoneSource, runner *fakeCancelRunner) *batchDeliveryImpl {
	impl, err := newBatchDelivery(batchDeliveryDeps{
		Owner: "owner-1", Done: done, Elect: &fakeElector{}, Runner: fakeStopOnlyRunner{runner},
	})
	if err != nil {
		panic(err)
	}
	return impl
}

// Acceptance (P1-1): a SYNC caller cancelled BEFORE the root completed submits
// Stop exactly ONCE, under a control context whose Err() is nil AT CAPTURE
// TIME, and returns the caller's ctx error promptly — the root is NOT settled
// by the Stop acknowledgement and repeated cancel never Stops twice.
func TestBatchDeliverySyncCallerCancelSubmitsStopOnce(t *testing.T) {
	t.Parallel()
	runner := newFakeCancelRunner()
	capCh := make(chan stopCapture, 1)
	go func() {
		cap := <-runner.gotCtx
		capCh <- cap
		close(runner.release)
	}()
	done := newFakeDone(fantasy.NewTextResponse("late")) // never finishes
	impl := cancelDeliveryImpl(done, runner)
	inv := deliveryTestInvocation(batchDeliverySync)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 1)
	go func() {
		_, err := impl.DeliverBatch(ctx, "root-1", inv)
		results <- err
	}()
	<-done.entered // AwaitDone ran: past the entry guard, now in the await.
	cancel()       // Caller gone while the root is still running: Stop must land.
	require.ErrorIs(t, <-results, context.Canceled, "the caller gets its own ctx error, never an envelope")
	require.Equal(t, int64(1), runner.stops.Load(), "exactly one Stop on caller cancellation")
	require.Equal(t, jobbatch.ID("root-1"), <-runner.stopRoot)
	cap := <-capCh
	require.NoError(t, cap.ctxErr, "Stop runs under a NON-cancelled control context (at capture time)")
	require.False(t, done.done, "the Stop acknowledgement never settles the root")
	// Repeated cancel: the second call returns at entry with no second Stop.
	_, err := impl.DeliverBatch(ctx, "root-1", inv)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, int64(1), runner.stops.Load(), "no second Stop on repeated cancel")
}

// ONLY sync cancels: an INLINE caller leaving must not Stop the root.
func TestBatchDeliveryInlineCallerCancelNeverStops(t *testing.T) {
	t.Parallel()
	runner := newFakeCancelRunner()
	done := newFakeDone(fantasy.NewTextResponse("late")) // never finishes
	impl := cancelDeliveryImpl(done, runner)
	inv := deliveryTestInvocation(batchDeliveryInline)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 1)
	go func() {
		_, err := impl.DeliverBatch(ctx, "root-1", inv)
		results <- err
	}()
	<-done.entered // Inline reaches AwaitDone via the started-winner path.
	cancel()
	require.ErrorIs(t, <-results, context.Canceled)
	require.Zero(t, runner.stops.Load(), "inline never submits Stop")
}

// ONLY sync cancels: a DETACHED caller leaving must not Stop the root.
func TestBatchDeliveryDetachedCallerCancelNeverStops(t *testing.T) {
	t.Parallel()
	runner := newFakeCancelRunner()
	done := newFakeDone(fantasy.NewTextResponse("late"))
	impl := cancelDeliveryImpl(done, runner)
	inv := deliveryTestInvocation(batchDeliveryDetached)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := impl.DeliverBatch(ctx, "root-1", inv)
	require.NoError(t, err)
	require.Zero(t, runner.stops.Load(), "detached never submits Stop")
}

// The root completed BEFORE the caller cancelled: no Stop is submitted.
func TestBatchDeliveryCompletedRootNeverStops(t *testing.T) {
	t.Parallel()
	runner := newFakeCancelRunner()
	envelope := fantasy.NewTextResponse("fast")
	done := newFakeDone(envelope)
	done.finish(envelope)
	impl := cancelDeliveryImpl(done, runner)
	inv := deliveryTestInvocation(batchDeliverySync)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancelled at entry, but the root already completed.
	_, err := impl.DeliverBatch(ctx, "root-1", inv)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, runner.stops.Load(), "a completed root needs no Stop")
}

// A Stop that returns an error neither hangs nor panics: the caller returns
// promptly with its ctx error joined with the Stop error.
func TestBatchDeliverySyncCancelStopErrorJoined(t *testing.T) {
	t.Parallel()
	runner := newFakeCancelRunner()
	runner.stopErr = errStopRefused
	go func() {
		<-runner.gotCtx
		close(runner.release)
	}()
	done := newFakeDone(fantasy.NewTextResponse("late"))
	impl := cancelDeliveryImpl(done, runner)
	inv := deliveryTestInvocation(batchDeliverySync)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 1)
	go func() {
		_, err := impl.DeliverBatch(ctx, "root-1", inv)
		results <- err
	}()
	<-done.entered // AwaitDone ran: past the entry guard, now in the await.
	cancel()
	err := <-results
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, errStopRefused, "the Stop error is joined alongside the ctx error")
	require.False(t, done.done, "still not settled")
}
