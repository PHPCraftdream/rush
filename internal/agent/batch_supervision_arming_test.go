package agent

// psv: tests for the batch-delivery supervision-arming restoration.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// psvArmRecorder records arm calls (owner, in order) under a mutex.
type psvArmRecorder struct {
	mu    sync.Mutex
	owner []string
}

func (r *psvArmRecorder) arm(_ context.Context, owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owner = append(r.owner, owner)
}

func (r *psvArmRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.owner...)
}

// psvJoiningDone records AwaitDone returns, including the delivery's
// background waiter in escape-window tests.
type psvJoiningDone struct {
	*fakeDoneSource
	returned chan struct{}
}

func psvNewJoiningDone(envelope fantasy.ToolResponse) *psvJoiningDone {
	return &psvJoiningDone{fakeDoneSource: newFakeDone(envelope), returned: make(chan struct{}, 4)}
}

func (f *psvJoiningDone) AwaitDone(ctx context.Context, owner string, root jobbatch.ID) (fantasy.ToolResponse, bool, error) {
	env, ok, err := f.fakeDoneSource.AwaitDone(ctx, owner, root)
	f.returned <- struct{}{}
	return env, ok, err
}

func psvDeliveryImplWithArming(t *testing.T, done batchDoneSource, elector batchElector, arm func(context.Context, string)) *batchDeliveryImpl {
	t.Helper()
	impl, err := newBatchDelivery(batchDeliveryDeps{Owner: "owner-1", Done: done, Elect: elector, Runner: noopDeliveryRunner(), ArmSupervision: arm})
	require.NoError(t, err)
	return impl
}

func psvArmingOverLedger(coord Coordinator) func(context.Context, string) {
	return PsvBatchSupervisionArmingLate(func() Coordinator { return coord })
}

func psvAssertArmed(l *workLedger, owner string) (*supervisionState, bool) {
	l.supervision.mu.Lock()
	defer l.supervision.mu.Unlock()
	st, ok := l.supervision.byRoot[owner]
	return st, ok
}

// psvLosingElector always loses the started election; optionally errors.
type psvLosingElector struct{ err error }

func (f psvLosingElector) ElectStarted(context.Context, string, jobbatch.ID) (bool, error) {
	return false, f.err
}

func (f psvLosingElector) ElectInline(context.Context, string, jobbatch.ID, fantasy.ToolResponse) (bool, error) {
	return false, nil
}

func TestBatchSupervisionArming_DetachedStartedArmsOnce(t *testing.T) {
	t.Parallel()
	done := newFakeDone(fantasy.NewTextResponse("later"))
	arm := &psvArmRecorder{}
	impl := psvDeliveryImplWithArming(t, done, &fakeElector{}, arm.arm)
	resp, err := impl.DeliverBatch(context.Background(), "root-1", deliveryTestInvocation(batchDeliveryDetached))
	require.NoError(t, err)
	require.Contains(t, resp.Content, "started")
	require.Len(t, arm.calls(), 1)
	require.Equal(t, []string{"owner-1"}, arm.calls())
	require.Zero(t, done.awaitCalls.Load())
}

func TestBatchSupervisionArming_DetachedLostElectionDoesNotArm(t *testing.T) {
	t.Parallel()
	arm := &psvArmRecorder{}
	impl := psvDeliveryImplWithArming(t, newFakeDone(fantasy.NewTextResponse("later")), psvLosingElector{}, arm.arm)
	_, err := impl.DeliverBatch(context.Background(), "root-1", deliveryTestInvocation(batchDeliveryDetached))
	require.NoError(t, err)
	require.Empty(t, arm.calls())
}

func TestBatchSupervisionArming_EscapeWindowElapseArms(t *testing.T) {
	t.Parallel()
	jdone := psvNewJoiningDone(fantasy.NewTextResponse("later"))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		select {
		case <-jdone.returned:
		case <-time.After(2 * time.Second):
			t.Errorf("the delivery's background AwaitDone goroutine never returned")
		}
	})
	elector := &fakeElector{}
	arm := &psvArmRecorder{}
	impl, err := newBatchDelivery(batchDeliveryDeps{
		Owner: "owner-1", Done: jdone, Elect: elector, Runner: noopDeliveryRunner(), ArmSupervision: arm.arm,
		Timer: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		},
	})
	require.NoError(t, err)
	inv := deliveryTestInvocation(batchDeliverySync)
	inv.BackgroundEscape = batchEscapeAuto
	inv.Delivery.Window = "1ms"
	resp, err := impl.DeliverBatch(ctx, "root-esc", inv)
	require.NoError(t, err)
	require.Contains(t, resp.Content, "started")
	require.Equal(t, int64(1), elector.startedCalls.Load())
	require.Equal(t, []string{"owner-1"}, arm.calls())
}

func TestBatchSupervisionArming_InWindowCompletionDoesNotArm(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("fast")
	jdone := psvNewJoiningDone(envelope)
	jdone.finish(envelope)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		select {
		case <-jdone.returned:
		case <-time.After(2 * time.Second):
			t.Errorf("the delivery's background AwaitDone goroutine never returned")
		}
	})
	elector := &fakeElector{}
	arm := &psvArmRecorder{}
	impl, err := newBatchDelivery(batchDeliveryDeps{
		Owner: "owner-1", Done: jdone, Elect: elector, Runner: noopDeliveryRunner(), ArmSupervision: arm.arm,
		Timer: func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	})
	require.NoError(t, err)
	inv := deliveryTestInvocation(batchDeliverySync)
	inv.BackgroundEscape = batchEscapeAuto
	inv.Delivery.Window = "10s"
	got, err := impl.DeliverBatch(ctx, "root-esc", inv)
	require.NoError(t, err)
	require.Equal(t, envelope, got)
	require.Zero(t, elector.startedCalls.Load())
	require.Empty(t, arm.calls())
}

func TestBatchSupervisionArming_SyncDeliveryDoesNotArm(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("done")
	done := newFakeDone(envelope)
	done.finish(envelope)
	arm := &psvArmRecorder{}
	impl := psvDeliveryImplWithArming(t, done, &fakeElector{}, arm.arm)
	got, err := impl.DeliverBatch(context.Background(), "root-1", deliveryTestInvocation(batchDeliverySync))
	require.NoError(t, err)
	require.Equal(t, envelope, got)
	require.Empty(t, arm.calls())
}

func TestBatchSupervisionArming_InlineAnsweredCompletionDoesNotArm(t *testing.T) {
	t.Parallel()
	envelope := fantasy.NewTextResponse("done")
	done := newFakeDone(envelope)
	elector := &fakeElector{}
	elector.hook = func() { done.finish(envelope) }
	arm := &psvArmRecorder{}
	impl := psvDeliveryImplWithArming(t, done, elector, arm.arm)
	got, err := impl.DeliverBatch(context.Background(), "root-1", deliveryTestInvocation(batchDeliveryInline))
	require.NoError(t, err)
	require.Equal(t, envelope, got)
	require.Equal(t, int64(1), elector.startedCalls.Load())
	require.Equal(t, int64(1), elector.inlineSuccesses.Load())
	require.Empty(t, arm.calls())
}

func TestBatchSupervisionArming_InlineHeldTerminationArms(t *testing.T) {
	t.Parallel()
	done := newFakeDone(fantasy.NewTextResponse("later"))
	done.awaitOK = false
	close(done.release)
	arm := &psvArmRecorder{}
	impl := psvDeliveryImplWithArming(t, done, &fakeElector{}, arm.arm)
	resp, err := impl.DeliverBatch(context.Background(), "root-1", deliveryTestInvocation(batchDeliveryInline))
	require.NoError(t, err)
	require.Contains(t, resp.Content, "started")
	require.Len(t, arm.calls(), 1)
}

func TestBatchSupervisionArming_InlineLostElectionDoesNotArm(t *testing.T) {
	t.Parallel()
	done := newFakeDone(fantasy.NewTextResponse("later"))
	done.awaitOK = false
	close(done.release)
	arm := &psvArmRecorder{}
	impl := psvDeliveryImplWithArming(t, done, psvLosingElector{}, arm.arm)
	resp, err := impl.DeliverBatch(context.Background(), "root-1", deliveryTestInvocation(batchDeliveryInline))
	require.NoError(t, err)
	require.Contains(t, resp.Content, "started")
	require.Empty(t, arm.calls())
}

func TestBatchSupervisionArming_FailedStartedElectionDoesNotArm(t *testing.T) {
	t.Parallel()
	electionErr := errors.New("tx failed")
	arm := &psvArmRecorder{}
	impl := psvDeliveryImplWithArming(t, newFakeDone(fantasy.NewTextResponse("later")), psvLosingElector{err: electionErr}, arm.arm)
	_, err := impl.DeliverBatch(context.Background(), "root-1", deliveryTestInvocation(batchDeliveryDetached))
	require.ErrorIs(t, err, electionErr)
	require.Empty(t, arm.calls())
}

func TestBatchSupervisionArming_PausedSessionResumedByStartedResponse(t *testing.T) {
	t.Parallel()
	l, coord := newSupervisionTestLedger(t)
	firstInv := deliveryTestInvocation(batchDeliveryDetached)
	firstInv.SessionID = "paused-owner"
	first := psvDeliveryImplWithArming(t, newFakeDone(fantasy.NewTextResponse("later")), &fakeElector{}, psvArmingOverLedger(coord))
	_, err := first.DeliverBatch(context.Background(), "paused-root-1", firstInv)
	require.NoError(t, err)
	l.supervision.mu.Lock()
	st, exists := l.supervision.byRoot["paused-owner"]
	l.supervision.mu.Unlock()
	require.True(t, exists, "first delivery must create the invoking owner's state")
	l.supervision.mu.Lock()
	st.paused = true
	st.tickCount = 1 << 20
	oldGeneration := st.generation
	baseInterval := st.cfg.Interval
	st.interval = 2 * baseInterval
	l.supervision.mu.Unlock()

	secondInv := deliveryTestInvocation(batchDeliveryDetached)
	secondInv.SessionID = "paused-owner"
	second := psvDeliveryImplWithArming(t, newFakeDone(fantasy.NewTextResponse("later")), &fakeElector{}, psvArmingOverLedger(coord))
	_, err = second.DeliverBatch(context.Background(), "paused-root-2", secondInv)
	require.NoError(t, err)
	l.supervision.mu.Lock()
	st = l.supervision.byRoot["paused-owner"]
	paused, ticks, interval, generation := st.paused, st.tickCount, st.interval, st.generation
	l.supervision.mu.Unlock()
	require.False(t, paused)
	require.Zero(t, ticks)
	require.Equal(t, baseInterval, interval)
	require.Greater(t, generation, oldGeneration)
}

func TestBatchSupervisionArming_DelegatedChildDoesNotArm(t *testing.T) {
	t.Parallel()
	l, coord := newSupervisionTestLedger(t)
	coord.subAgentDrivers.register("child-owner", subAgentDriver{agent: &mockSessionAgent{}})
	childInv := deliveryTestInvocation(batchDeliveryDetached)
	childInv.SessionID = "child-owner"
	child := psvDeliveryImplWithArming(t, newFakeDone(fantasy.NewTextResponse("later")), &fakeElector{}, psvArmingOverLedger(coord))
	_, err := child.DeliverBatch(context.Background(), "child-root", childInv)
	require.NoError(t, err)
	_, childArmed := psvAssertArmed(l, "child-owner")
	require.False(t, childArmed)

	rootInv := deliveryTestInvocation(batchDeliveryDetached)
	rootInv.SessionID = "root-owner"
	root := psvDeliveryImplWithArming(t, newFakeDone(fantasy.NewTextResponse("later")), &fakeElector{}, psvArmingOverLedger(coord))
	_, err = root.DeliverBatch(context.Background(), "root-root", rootInv)
	require.NoError(t, err)
	_, rootArmed := psvAssertArmed(l, "root-owner")
	require.True(t, rootArmed, "a non-child delivery must arm its invoking owner")
}

func TestBatchSupervisionArming_DisabledByCallOptionsDoesNotArm(t *testing.T) {
	t.Parallel()
	l, coord := newSupervisionTestLedger(t)
	disabledInv := deliveryTestInvocation(batchDeliveryDetached)
	disabledInv.SessionID = "disabled-owner"
	disabled := psvDeliveryImplWithArming(t, newFakeDone(fantasy.NewTextResponse("later")), &fakeElector{}, psvArmingOverLedger(coord))
	ctx := WithCallOptions(context.Background(), &CallOptions{SupervisionDisabled: true})
	_, err := disabled.DeliverBatch(ctx, "disabled-root", disabledInv)
	require.NoError(t, err)
	_, disabledArmed := psvAssertArmed(l, "disabled-owner")
	require.False(t, disabledArmed)

	enabledInv := deliveryTestInvocation(batchDeliveryDetached)
	enabledInv.SessionID = "enabled-owner"
	enabled := psvDeliveryImplWithArming(t, newFakeDone(fantasy.NewTextResponse("later")), &fakeElector{}, psvArmingOverLedger(coord))
	_, err = enabled.DeliverBatch(context.Background(), "enabled-root", enabledInv)
	require.NoError(t, err)
	_, enabledArmed := psvAssertArmed(l, "enabled-owner")
	require.True(t, enabledArmed)

	intervalInv := deliveryTestInvocation(batchDeliveryDetached)
	intervalInv.SessionID = "interval-owner"
	interval := psvDeliveryImplWithArming(t, newFakeDone(fantasy.NewTextResponse("later")), &fakeElector{}, psvArmingOverLedger(coord))
	intervalCtx := WithCallOptions(context.Background(), &CallOptions{SupervisionInterval: 7 * time.Second})
	_, err = interval.DeliverBatch(intervalCtx, "interval-root", intervalInv)
	require.NoError(t, err)
	intervalState, intervalArmed := psvAssertArmed(l, "interval-owner")
	require.True(t, intervalArmed)
	require.Equal(t, 7*time.Second, intervalState.cfg.Interval)
}

func TestBatchSupervisionArming_DisabledByConfigDoesNotArm(t *testing.T) {
	t.Parallel()
	l, coord := newSupervisionTestLedger(t)
	coord.cfg = config.NewLibraryStore(&config.Config{Options: &config.Options{SupervisionEnabled: boolPtr(false)}}, t.TempDir())
	disabledInv := deliveryTestInvocation(batchDeliveryDetached)
	disabledInv.SessionID = "cfg-disabled-owner"
	disabled := psvDeliveryImplWithArming(t, newFakeDone(fantasy.NewTextResponse("later")), &fakeElector{}, psvArmingOverLedger(coord))
	_, err := disabled.DeliverBatch(context.Background(), "cfg-root", disabledInv)
	require.NoError(t, err)
	_, armed := psvAssertArmed(l, "cfg-disabled-owner")
	require.False(t, armed)

	coord.cfg = config.NewLibraryStore(&config.Config{Options: &config.Options{SupervisionEnabled: boolPtr(true)}}, t.TempDir())
	enabledInv := deliveryTestInvocation(batchDeliveryDetached)
	enabledInv.SessionID = "cfg-enabled-owner"
	enabled := psvDeliveryImplWithArming(t, newFakeDone(fantasy.NewTextResponse("later")), &fakeElector{}, psvArmingOverLedger(coord))
	_, err = enabled.DeliverBatch(context.Background(), "cfg-root-on", enabledInv)
	require.NoError(t, err)
	_, enabledArmed := psvAssertArmed(l, "cfg-enabled-owner")
	require.True(t, enabledArmed)
}

func TestBatchSupervisionArming_LateUnboundCoordinatorIsNoOp(t *testing.T) {
	t.Parallel()
	l, coord := newSupervisionTestLedger(t)
	var typedNil *coordinator
	arms := []func(context.Context, string){
		PsvBatchSupervisionArmingLate(nil),
		PsvBatchSupervisionArmingLate(func() Coordinator { return nil }),
		PsvBatchSupervisionArmingLate(func() Coordinator { return typedNil }),
	}
	require.NotPanics(t, func() {
		for _, arm := range arms {
			arm(context.Background(), "orphan-root")
			arm(context.Background(), "")
		}
		PsvBatchSupervisionArmingLate(func() Coordinator { return coord })(context.Background(), "")
	})
	l.supervision.mu.Lock()
	remaining := len(l.supervision.byRoot)
	l.supervision.mu.Unlock()
	require.Zero(t, remaining)
}
