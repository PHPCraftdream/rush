package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// fakeDoneSource is the completion seam of the delivery tests. The done flag
// flips under a barrier; AwaitDone blocks on a release gate so tests control
// settlement with signals, never sleeps.
type fakeDoneSource struct {
	mu       sync.Mutex
	done     bool
	envelope fantasy.ToolResponse
	// release gates AwaitDone; closed after done flips in tests that wait.
	release chan struct{}
	// entered receives a signal each time AwaitDone is entered (non-blocking
	// send): lets tests gate on "the await actually ran" without sleeps.
	entered    chan struct{}
	awaitCalls atomic.Int64
	loadCalls  atomic.Int64
	awaitOK    bool
	awaitErr   error
}

func newFakeDone(envelope fantasy.ToolResponse) *fakeDoneSource {
	return &fakeDoneSource{envelope: envelope, release: make(chan struct{}), entered: make(chan struct{}, 16)}
}

func (f *fakeDoneSource) finish(env fantasy.ToolResponse) {
	f.mu.Lock()
	f.done = true
	f.envelope = env
	f.mu.Unlock()
	close(f.release)
}

func (f *fakeDoneSource) AwaitDone(ctx context.Context, _ string, _ jobbatch.ID) (fantasy.ToolResponse, bool, error) {
	f.awaitCalls.Add(1)
	select {
	case f.entered <- struct{}{}:
	default:
	}
	select {
	case <-f.release:
	case <-ctx.Done():
		return fantasy.ToolResponse{}, false, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.awaitErr != nil {
		return fantasy.ToolResponse{}, false, f.awaitErr
	}
	return f.envelope, f.awaitOK || f.done, nil
}

func (f *fakeDoneSource) LoadDone(_ context.Context, _ string, _ jobbatch.ID) (fantasy.ToolResponse, bool, error) {
	f.loadCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.envelope, f.done, nil
}

// fakeElector elects via sync.Once-style CAS: exactly one caller wins each
// election, losers get false with no error.
type fakeElector struct {
	startedOnce, inlineOnce   sync.Once
	startedWin, inlineWin     atomic.Bool
	startedCalls, inlineCalls atomic.Int64
	inlineErrs                atomic.Int64
	inlineFailFirst           int // First N ElectInline calls fail (failed ack).
	inlineSuccesses           atomic.Int64
	hook                      func() // Optional barrier hook run on first wins.
}

func (f *fakeElector) ElectStarted(_ context.Context, _ string, _ jobbatch.ID) (bool, error) {
	f.startedCalls.Add(1)
	won := false
	f.startedOnce.Do(func() {
		won = true
		f.startedWin.Store(true)
		if f.hook != nil {
			f.hook()
		}
	})
	return won, nil
}

func (f *fakeElector) ElectInline(_ context.Context, _ string, _ jobbatch.ID, _ fantasy.ToolResponse) (bool, error) {
	f.inlineCalls.Add(1)
	if n := int(f.inlineCalls.Load()); n <= f.inlineFailFirst {
		f.inlineErrs.Add(1)
		return false, errors.New("tx failed")
	}
	won := false
	f.inlineOnce.Do(func() { won = true; f.inlineWin.Store(true) })
	if won {
		f.inlineSuccesses.Add(1)
	}
	return won, nil
}

func deliveryTestInvocation(mode string) BatchInvocation {
	inv := BatchInvocation{
		SessionID: "owner-1", MessageID: "msg-1", RegistryID: "reg-1",
		Origin: "test", WaiterRank: 0,
		Policy: batchLeafPolicyContext{RegistryID: "reg-1", RestrictedToolset: "standard"},
	}
	inv.Delivery.Mode = mode
	return inv
}

// noopDeliveryRunner is a Runner handle for tests that never cancel: Stop is
// never reached.
func noopDeliveryRunner() batchRunnerHandle {
	return fakeStopOnlyRunner{newFakeCancelRunner()}
}

func deliveryImpl(t *testing.T, done *fakeDoneSource, elector *fakeElector) *batchDeliveryImpl {
	t.Helper()
	impl, err := newBatchDelivery(batchDeliveryDeps{Owner: "owner-1", Done: done, Elect: elector, Runner: noopDeliveryRunner()})
	require.NoError(t, err)
	return impl
}

// Acceptance: sync waits for the root's real completion and returns its tool
// response. Text, image/audio data + media type, metadata and IsError are
// preserved — never collapsed to text-only.
func TestBatchDeliverySyncPreservesEnvelope(t *testing.T) {
	t.Parallel()
	envelope := fantasy.ToolResponse{
		Type: "image", Content: "screenshot",
		Data: []byte{1, 2, 3}, MediaType: "image/png",
		Metadata: `{"cost":1}`, IsError: true,
	}
	done := newFakeDone(envelope)
	done.finish(envelope)
	impl := deliveryImpl(t, done, &fakeElector{})
	got, err := impl.deliverSync(context.Background(), "owner-1", "root-1", "")
	require.NoError(t, err)
	require.Equal(t, envelope, got, "sync must forward the full executor envelope")
}

func TestBatchDeliverySyncUnavailableEnvelope(t *testing.T) {
	t.Parallel()
	done := newFakeDone(fantasy.ToolResponse{})
	done.awaitOK = false
	impl, err := newBatchDelivery(batchDeliveryDeps{Owner: "owner-1", Done: done, Elect: &fakeElector{}, Runner: noopDeliveryRunner()})
	require.NoError(t, err)
	// Never released: close with no completion -> unavailable, never settled.
	close(done.release)
	_, err = impl.deliverSync(context.Background(), "owner-1", "root-1", "")
	require.ErrorIs(t, err, errBatchDoneUnavailable)
}

// Acceptance: finish-before-ack — the root finished before the caller's ack —
// still delivers exactly one completion.
func TestBatchDeliveryInlineFinishBeforeAck(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("done")
	done := newFakeDone(envelope)
	elector := fakeElector{}
	impl, err := newBatchDelivery(batchDeliveryDeps{Owner: "owner-1", Done: done, Elect: &elector, Runner: noopDeliveryRunner()})
	require.NoError(t, err)
	done.finish(envelope)
	got, err := impl.DeliverBatch(context.Background(), "root-1", deliveryTestInvocation(batchDeliveryInline))
	require.NoError(t, err)
	require.Equal(t, envelope, got)
	require.True(t, elector.inlineWin.Load(), "winner elected inline")
	require.Equal(t, int64(1), elector.inlineSuccesses.Load(), "exactly one inline election")
	require.Zero(t, elector.startedCalls.Load(), "a settled root needs no started history")
}

// Acceptance: a failed ack (election transaction error) neither loses nor
// duplicates the completion; the bounded retry succeeds.
func TestBatchDeliveryInlineFailedAckRetries(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("done")
	done := newFakeDone(envelope)
	elector := fakeElector{inlineFailFirst: 2}
	impl := deliveryImpl(t, done, &elector)
	done.finish(envelope)
	got, err := impl.DeliverBatch(context.Background(), "root-1", deliveryTestInvocation(batchDeliveryInline))
	require.NoError(t, err)
	require.Equal(t, envelope, got)
	require.Equal(t, int64(1), elector.inlineSuccesses.Load(), "failed acks never duplicate the completion")
	require.Equal(t, int64(3), elector.inlineCalls.Load())
}

// A persistently failing ack is a typed error; the completion stays durable
// and unannounced (LoadDone still reports it), so it is not lost.
func TestBatchDeliveryInlineFailedAckExhausted(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("done")
	done := newFakeDone(envelope)
	elector := fakeElector{inlineFailFirst: batchInlineElectionAttempts}
	impl := deliveryImpl(t, done, &elector)
	done.finish(envelope)
	_, err := impl.DeliverBatch(context.Background(), "root-1", deliveryTestInvocation(batchDeliveryInline))
	require.ErrorIs(t, err, errBatchInlineElectionFailed)
	require.Zero(t, elector.inlineSuccesses.Load(), "no duplicate election committed")
	require.True(t, done.done, "completion stays durable for the outbox consumers")
}

// Acceptance: the inline-vs-started race resolves to exactly one elected
// delivery — one started history winner and one completion winner; the
// losers return the same envelope as a read.
func TestBatchDeliveryInlineVsStartedRace(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("done")
	done := newFakeDone(envelope)
	elector := fakeElector{}
	impl := deliveryImpl(t, done, &elector)
	// Both callers start before the root settles; then it settles once.
	launch := make(chan struct{})
	const callers = 2
	type result struct {
		resp fantasy.ToolResponse
		err  error
	}
	results := make(chan result, callers)
	var entering sync.WaitGroup
	entering.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			entering.Done()
			<-launch
			resp, err := impl.DeliverBatch(context.Background(), "root-1", deliveryTestInvocation(batchDeliveryInline))
			results <- result{resp, err}
		}()
	}
	entering.Wait()
	close(launch)
	// Both callers entered AwaitDone: each passed LoadDone-false, elected
	// (one started CAS winner, one loser) and is awaiting the release gate.
	// Settling only now makes the outcome deterministic.
	<-done.entered
	<-done.entered
	done.finish(envelope)
	for i := 0; i < callers; i++ {
		r := <-results
		require.NoError(t, r.err)
		require.Equal(t, envelope, r.resp)
	}
	require.True(t, elector.startedWin.Load(), "exactly one started history")
	require.Equal(t, int64(1), elector.inlineSuccesses.Load(), "exactly one completion delivery")
	require.Equal(t, int64(1), elector.inlineCalls.Load(), "the started loser returns the envelope read-only")
}

// Acceptance: detached returns the started response immediately and never
// blocks on the root's completion.
func TestBatchDeliveryDetachedNeverBlocks(t *testing.T) {
	t.Parallel()
	done := newFakeDone(fantasy.NewTextResponse("later"))
	elector := fakeElector{}
	impl := deliveryImpl(t, done, &elector)
	resp, err := impl.DeliverBatch(context.Background(), "root-1", deliveryTestInvocation(batchDeliveryDetached))
	require.NoError(t, err)
	require.Contains(t, resp.Content, "started", "detached returns the started response")
	require.Zero(t, done.awaitCalls.Load(), "detached never waits for completion")
	require.Equal(t, int64(1), elector.startedCalls.Load())
}

// Acceptance: escape=auto with a completion INSIDE the window returns the
// full envelope with NO started election — the old always-detached shortcut
// is gone; a 10 ms command no longer reports "started".
func TestBatchDeliveryEscapeCompletesWithinWindow(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("fast")
	done := newFakeDone(envelope)
	elector := fakeElector{}
	impl := deliveryImpl(t, done, &elector)
	inv := deliveryTestInvocation(batchDeliverySync)
	inv.BackgroundEscape = batchEscapeAuto
	inv.Delivery.Window = "10s"
	done.finish(envelope)
	got, err := impl.DeliverBatch(context.Background(), "root-esc", inv)
	require.NoError(t, err)
	require.Equal(t, envelope, got, "completion inside the window returns the full envelope")
	require.Zero(t, elector.startedCalls.Load(), "no ElectStarted for an in-window completion")
}

// Acceptance: escape=auto with the window ELAPSING elects the started history
// once and returns the started response without Stop, without a second Submit
// and without waiting for the real Done (timer seam, no sleeps).
func TestBatchDeliveryEscapeWindowElapsed(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(nil)
	root, err := runner.Submit(context.Background(), jobbatch.SubmitRequest{Root: jobbatch.ID("root-esc")})
	require.NoError(t, err)
	submitted := runner.submitCalls()
	done := newFakeDone(fantasy.NewTextResponse("later")) // never finishes
	t.Cleanup(func() { close(done.release) })
	elector := fakeElector{}
	impl, err := newBatchDelivery(batchDeliveryDeps{
		Owner: "owner-1", Done: done, Elect: &elector, Runner: fakeStopOnlyRunner{newFakeCancelRunner()},
		// The window may only elapse AFTER AwaitDone was entered: the timer
		// fires once the await goroutine actually ran.
		Timer: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			go func() { <-done.entered; ch <- time.Now() }()
			return ch
		},
	})
	require.NoError(t, err)
	inv := deliveryTestInvocation(batchDeliverySync)
	inv.BackgroundEscape = batchEscapeAuto
	inv.Delivery.Window = "500ms"
	resp, err := impl.DeliverBatch(context.Background(), root, inv)
	require.NoError(t, err)
	require.Contains(t, resp.Content, "started", "window elapse returns the started response")
	require.Equal(t, int64(1), elector.startedCalls.Load(), "exactly one started election")
	require.Equal(t, submitted, runner.submitCalls(), "escape never launches a second root")
	require.Equal(t, int64(1), done.awaitCalls.Load(), "escape waits for the real Done first")
}

func TestBatchDeliveryEscapeSyncCancelSubmitsStop(t *testing.T) {
	t.Parallel()
	runner := newFakeCancelRunner()
	stopErr := errors.New("stop failed")
	runner.stopErr = stopErr
	done := newFakeDone(fantasy.NewTextResponse("late"))
	impl, err := newBatchDelivery(batchDeliveryDeps{
		Owner: "owner-1", Done: done, Elect: &fakeElector{}, Runner: fakeStopOnlyRunner{runner},
		Timer: func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inv := deliveryTestInvocation(batchDeliverySync)
	inv.BackgroundEscape = batchEscapeAuto
	inv.Delivery.Window = "1s"
	results := make(chan error, 1)
	go func() {
		_, err := impl.DeliverBatch(ctx, "root-esc", inv)
		results <- err
	}()
	<-done.entered
	cancel()
	select {
	case root := <-runner.stopRoot:
		require.Equal(t, jobbatch.ID("root-esc"), root)
	case <-time.After(2 * time.Second):
		t.Fatal("sync cancellation did not submit Stop")
	}
	select {
	case err := <-results:
		t.Fatalf("sync cancellation returned before Stop completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(runner.release)
	select {
	case err := <-results:
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, err, stopErr, "Stop failure must be joined with caller cancellation")
	case <-time.After(2 * time.Second):
		t.Fatal("escape-window delivery did not return after Stop completed")
	}
	require.Equal(t, int64(1), runner.stops.Load(), "sync cancellation must submit exactly one Stop")
}

// Acceptance: window elapse racing completion commits to exactly ONE outcome —
// either the full envelope with zero started elections, or the started
// response with exactly one election. Never both.
func TestBatchDeliveryEscapeRacingOutcome(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("racy")
	done := newFakeDone(envelope)
	elector := fakeElector{}
	fired := make(chan time.Time, 1)
	fired <- time.Now()
	impl, err := newBatchDelivery(batchDeliveryDeps{
		Owner: "owner-1", Done: done, Elect: &elector, Runner: noopDeliveryRunner(),
		Timer: func(time.Duration) <-chan time.Time { return fired },
	})
	require.NoError(t, err)
	inv := deliveryTestInvocation(batchDeliverySync)
	inv.BackgroundEscape = batchEscapeAuto
	inv.Delivery.Window = "1ms"
	done.finish(envelope) // Both select branches ready before entry.
	got, err := impl.DeliverBatch(context.Background(), "root-esc", inv)
	require.NoError(t, err)
	if reflect.DeepEqual(got, envelope) {
		require.Zero(t, elector.startedCalls.Load(), "envelope outcome must have zero started elections")
	} else {
		require.Contains(t, got.Content, "started")
		require.Equal(t, int64(1), elector.startedCalls.Load(), "started outcome must have exactly one election")
	}
}

// Acceptance: escape=auto without a window is a TYPED ERROR, never an
// implicit default; a malformed window is a different typed error.
func TestBatchDeliveryEscapeWindowTypedErrors(t *testing.T) {
	t.Parallel()
	impl := deliveryImpl(t, newFakeDone(fantasy.NewTextResponse("x")), &fakeElector{})
	for _, tc := range []struct {
		window string
		err    error
	}{{"", errBatchEscapeWindowRequired}, {"soon", errBatchEscapeWindowInvalid}, {"0s", errBatchEscapeWindowInvalid}} {
		inv := deliveryTestInvocation(batchDeliverySync)
		inv.BackgroundEscape = batchEscapeAuto
		inv.Delivery.Window = tc.window
		_, err := impl.DeliverBatch(context.Background(), "root-esc", inv)
		require.ErrorIs(t, err, tc.err, "window %q", tc.window)
	}
}

// Acceptance: escape=never ignores the window entirely — an empty window
// still delivers synchronously.
func TestBatchDeliveryEscapeNeverIgnoresWindow(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("done")
	done := newFakeDone(envelope)
	impl := deliveryImpl(t, done, &fakeElector{})
	inv := deliveryTestInvocation(batchDeliverySync)
	inv.BackgroundEscape = batchEscapeNever
	inv.Delivery.Window = ""
	done.finish(envelope)
	got, err := impl.DeliverBatch(context.Background(), "root-esc", inv)
	require.NoError(t, err)
	require.Equal(t, envelope, got, "escape=never ignores the window")
}

// Acceptance: escape=auto with Mode detached behaves as detached.
func TestBatchDeliveryEscapeAutoDetachedStaysDetached(t *testing.T) {
	t.Parallel()
	done := newFakeDone(fantasy.NewTextResponse("later"))
	elector := fakeElector{}
	impl := deliveryImpl(t, done, &elector)
	inv := deliveryTestInvocation(batchDeliveryDetached)
	inv.BackgroundEscape = batchEscapeAuto
	resp, err := impl.DeliverBatch(context.Background(), "root-esc", inv)
	require.NoError(t, err)
	require.Contains(t, resp.Content, "started")
	require.Zero(t, done.awaitCalls.Load())
	require.Equal(t, int64(1), elector.startedCalls.Load())
}

func TestBatchDeliveryConstructorFailsClosed(t *testing.T) {
	t.Parallel()
	_, err := newBatchDelivery(batchDeliveryDeps{})
	require.ErrorIs(t, err, errBatchDeliveryDeps)
	// The Runner is REQUIRED: without it a cancelled sync caller could not
	// submit Stop.
	_, err = newBatchDelivery(batchDeliveryDeps{Owner: "owner-1", Done: newFakeDone(fantasy.ToolResponse{}), Elect: &fakeElector{}})
	require.ErrorIs(t, err, errBatchDeliveryDeps)
	_, err = newBatchDelivery(batchDeliveryDeps{Owner: "", Done: newFakeDone(fantasy.ToolResponse{}), Elect: &fakeElector{}, Runner: noopDeliveryRunner()})
	require.ErrorIs(t, err, errBatchDeliveryOwner)
}

func TestBatchDeliveryUnknownMode(t *testing.T) {
	t.Parallel()
	impl, err := newBatchDelivery(batchDeliveryDeps{Owner: "owner-1", Done: newFakeDone(fantasy.ToolResponse{}), Elect: &fakeElector{}, Runner: noopDeliveryRunner()})
	require.NoError(t, err)
	inv := deliveryTestInvocation("mystery")
	_, err = impl.DeliverBatch(context.Background(), "root-1", inv)
	require.ErrorIs(t, err, errBatchDeliveryUnknownMode)
}
