package agent

// Delivery-arming and delivery-seam regressions. T1 drives the PRODUCTION
// assembly (newBatchWorkToolBuild, both modes) and pins the per-family
// delivery contract; T2–T5 drive the production batchDeliveryImpl over
// in-file fakes and pin the detached/windowed delivery properties: detached
// NEVER awaits, loads, submits or stops; the windowed escape races the real
// completion against the injectable timer with no sleeps.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// gatedDoneSource is a batchDoneSource fake: with a completion armed it
// returns it immediately; otherwise AwaitDone blocks on the gate until it is
// released with the armed envelope. Both methods count calls atomically.
type gatedDoneSource struct {
	mu         sync.Mutex
	awaitCalls int
	loadCalls  int
	gate       chan struct{}
	gateClosed bool
	env        fantasy.ToolResponse
}

func newGatedDoneSource() *gatedDoneSource {
	return &gatedDoneSource{gate: make(chan struct{})}
}

// arm records the completion and opens the gate, so a waiter that already
// blocked on it and a waiter that arrives later both receive the envelope.
func (g *gatedDoneSource) arm(env fantasy.ToolResponse) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.env = env
	g.closeGateLocked()
}

func (g *gatedDoneSource) closeGateLocked() {
	if !g.gateClosed {
		close(g.gate)
		g.gateClosed = true
	}
}

func (g *gatedDoneSource) AwaitDone(context.Context, string, jobbatch.ID) (fantasy.ToolResponse, bool, error) {
	g.mu.Lock()
	g.awaitCalls++
	gate := g.gate
	g.mu.Unlock()
	<-gate
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.env, true, nil
}

func (g *gatedDoneSource) LoadDone(context.Context, string, jobbatch.ID) (fantasy.ToolResponse, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loadCalls++
	return fantasy.ToolResponse{}, false, nil
}

func (g *gatedDoneSource) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closeGateLocked()
}

func (g *gatedDoneSource) awaitCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.awaitCalls
}

func (g *gatedDoneSource) loadCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.loadCalls
}

// countingElector is a batchElector fake: every election wins and every call
// is counted.
type countingElector struct {
	mu      sync.Mutex
	started int
	inline  int
}

func (e *countingElector) ElectStarted(context.Context, string, jobbatch.ID) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.started++
	return true, nil
}

func (e *countingElector) ElectInline(context.Context, string, jobbatch.ID, fantasy.ToolResponse) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inline++
	return true, nil
}

func (e *countingElector) startedCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.started
}

func (e *countingElector) inlineCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inline
}

// recordingRunner is a batchRunnerHandle fake: Submit and Stop are recorded
// and return zero values; the delivery seam must never call either.
type recordingRunner struct {
	mu      sync.Mutex
	submits int
	stops   int
}

func (r *recordingRunner) Submit(context.Context, jobbatch.SubmitRequest) (jobbatch.ID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.submits++
	return jobbatch.ID("root-1"), nil
}

func (r *recordingRunner) Stop(context.Context, jobbatch.ID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops++
	return nil
}

func (r *recordingRunner) submitCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.submits
}

func (r *recordingRunner) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stops
}

// detachedInvocation is the EXACT invocation shape the agent and
// agentic_fetch families are armed with by newBatchWorkToolBuild.
func detachedInvocation() BatchInvocation {
	return BatchInvocation{
		SessionID:        "sess-1",
		Delivery:         batchDeliveryChoice{Mode: batchDeliveryDetached},
		BackgroundEscape: batchEscapeNever,
	}
}

func newTestDelivery(done batchDoneSource) (*batchDeliveryImpl, *countingElector, *recordingRunner) {
	elector := &countingElector{}
	runner := &recordingRunner{}
	impl, err := newBatchDelivery(batchDeliveryDeps{
		Owner: "default-owner", Done: done, Elect: elector, Runner: runner,
	})
	if err != nil {
		panic(fmt.Sprintf("newBatchDelivery: %v", err))
	}
	return impl, elector, runner
}

// TestBatchWorkFamilyDeliveryArming drives the production assembly in BOTH
// modes and pins the per-family delivery contract. The first assertion (the
// agent family's detached mode) catches a mutation of the agent family back
// to batchDeliverySync, which reintroduces the parent-turn deadlock (finding
// 2 of the review record): the parent's tool call would block until the
// delegated child turn completes.
func TestBatchWorkFamilyDeliveryArming(t *testing.T) {
	t.Parallel()
	names := []string{tools.BashToolName, AgentToolName, tools.AgenticFetchToolName, "view"}
	for _, deferred := range []bool{true, false} {
		t.Run(fmt.Sprintf("deferred=%v", deferred), func(t *testing.T) {
			t.Parallel()
			bind := &batchCoordinatorBinding{
				ingress:      &fakeBatchIngress{},
				diskBindings: NewDiskProviderRegistry(),
			}
			built, err := newBatchWorkToolBuild(bind, batchTestTools(names...), newBuildResourceScope(), deferred)
			require.NoError(t, err)
			armed := map[string]*batchIngressTool{}
			for _, tool := range built.tools {
				if ing, ok := tool.(*batchIngressTool); ok {
					armed[tool.Info().Name] = ing
				}
			}
			bash := armed[tools.BashToolName]
			require.NotNil(t, bash, "bash must be armed")
			require.Equal(t, batchDeliverySync, bash.inv.Delivery.Mode)
			require.Equal(t, batchEscapeAuto, bash.inv.BackgroundEscape)
			require.Equal(t, inlineWindow.String(), bash.inv.Delivery.Window)

			// FIRST assertion: the delegation families are DETACHED.
			for _, name := range []string{AgentToolName, tools.AgenticFetchToolName} {
				ing := armed[name]
				require.NotNil(t, ing, "%s must be armed", name)
				require.Equal(t, batchDeliveryDetached, ing.inv.Delivery.Mode,
					"%s must be detached: sync reintroduces the parent-turn deadlock", name)
				require.Equal(t, batchEscapeNever, ing.inv.BackgroundEscape)
				require.Empty(t, ing.inv.Delivery.Window)
			}
			view := armed["view"]
			require.NotNil(t, view, "view must be armed")
			require.Equal(t, batchDeliverySync, view.inv.Delivery.Mode)
			require.Equal(t, batchEscapeNever, view.inv.BackgroundEscape)
			require.Empty(t, view.inv.Delivery.Window)
		})
	}
}

// TestDetachedDeliveryNeverAwaitsChild pins the wedge property: the delivery
// seam returns the started text without ever awaiting, loading, submitting
// or stopping. The first assertion catches a mutation of deliverDetached
// back to deliverSync, which makes the parent block until the child
// completes — a gated child would hang the run forever.
func TestDetachedDeliveryNeverAwaitsChild(t *testing.T) {
	t.Parallel()
	done := newGatedDoneSource()
	impl, elector, runner := newTestDelivery(done)
	resp, err := impl.DeliverBatch(context.Background(), "root-1", detachedInvocation())
	require.NoError(t, err)
	require.Equal(t,
		fmt.Sprintf("batch root %s %s", jobbatch.ID("root-1"), batchStartedResponseSuffix),
		resp.Content,
		"detached must return the started text without awaiting (deliverSync would block on the gated child)")
	require.Zero(t, done.awaitCount(), "detached must never AwaitDone")
	require.Zero(t, done.loadCount(), "detached must never LoadDone")
	require.Zero(t, runner.submitCount(), "the delivery seam never submits")
	require.Zero(t, runner.stopCount(), "the delivery seam never stops on detached delivery")
	require.Equal(t, 1, elector.startedCount(), "exactly one started election")
	// Cleanliness: releasing the gate after the fact changes nothing at the
	// delivery layer.
	done.arm(fantasy.NewTextResponse("done"))
	done.release()
	require.Zero(t, runner.stopCount())
	require.Zero(t, runner.submitCount())
}

// TestHeldQuestionDoesNotWedgeParentDelivery: the child is blocked in a
// question; the parent's detached call still returns started immediately,
// and the later answer arrival neither cancels nor relaunches anything. The
// first assertion catches a mutation back to sync: the parent's tool call
// then blocks on the held question and the rush run hangs
// (TestRunNonInteractive_ChildQuestionRoundTrip deadlock).
func TestHeldQuestionDoesNotWedgeParentDelivery(t *testing.T) {
	t.Parallel()
	done := newGatedDoneSource()
	impl, elector, runner := newTestDelivery(done)
	resp, err := impl.DeliverBatch(context.Background(), "root-1", detachedInvocation())
	require.NoError(t, err)
	require.Equal(t,
		fmt.Sprintf("batch root %s %s", jobbatch.ID("root-1"), batchStartedResponseSuffix),
		resp.Content,
		"detached must return started while the child question is held")
	require.Zero(t, elector.inlineCount())
	// Detached never awaited even the held question; the answer therefore
	// reaches no waiter at the delivery layer at all.
	require.Zero(t, done.awaitCount(), "detached never awaits the held question")

	// The answer arrives: release the gate with the completion envelope.
	done.arm(fantasy.NewTextResponse("answered"))
	done.release()
	require.Zero(t, runner.stopCount(), "the answer injection never cancels")
	require.Zero(t, runner.submitCount(), "the answer injection never relaunches")
	require.Zero(t, elector.inlineCount(), "no additional election after the answer")

	// A SECOND delivery for the same root still returns started and still
	// never submits or stops: no second delegation incarnation at the
	// delivery layer.
	resp, err = impl.DeliverBatch(context.Background(), "root-1", detachedInvocation())
	require.NoError(t, err)
	require.Equal(t,
		fmt.Sprintf("batch root %s %s", jobbatch.ID("root-1"), batchStartedResponseSuffix),
		resp.Content)
	require.Zero(t, runner.submitCount())
	require.Zero(t, runner.stopCount())
}

// TestFastCompletionBeforeStartedAck: the root completes BEFORE the parent's
// ack. Detached still elects started first and returns only the started
// text; AwaitDone is never called, so the completion reaches the parent only
// via selected notices, never as a second in-call execution. This catches a
// mutation where detached also awaits or loads done and double-delivers.
func TestFastCompletionBeforeStartedAck(t *testing.T) {
	t.Parallel()
	done := newGatedDoneSource()
	done.arm(fantasy.NewTextResponse("fast-done"))
	impl, elector, runner := newTestDelivery(done)
	resp, err := impl.DeliverBatch(context.Background(), "root-1", detachedInvocation())
	require.NoError(t, err)
	require.Equal(t,
		fmt.Sprintf("batch root %s %s", jobbatch.ID("root-1"), batchStartedResponseSuffix),
		resp.Content,
		"detached always elects started before any completion delivery")
	require.Equal(t, 1, elector.startedCount())
	require.Zero(t, elector.inlineCount())
	require.Zero(t, done.awaitCount(), "detached must not await even a ready completion")
	require.Zero(t, done.loadCount(), "detached must not load done")
	require.Zero(t, runner.submitCount())
	require.Zero(t, runner.stopCount())
}

// TestBashEscapeWindowTwins pins the windowed escape for the bash family's
// armed shape (sync + escape auto) with a pre-created, manually fired timer
// channel — no sleeps. Twin 1: completion inside the window returns the full
// envelope unchanged with NO started election (the timer channel is never
// fired). Twin 2: window elapse (fired from the test while DeliverBatch runs
// on a goroutine) elects started and returns the started text without Stop,
// without a second Submit, and with exactly one AwaitDone waiter that later
// receives the real completion.
func TestBashEscapeWindowTwins(t *testing.T) {
	t.Parallel()
	buildDelivery := func(done batchDoneSource) (*batchDeliveryImpl, *countingElector, *recordingRunner, chan time.Time) {
		elector := &countingElector{}
		runner := &recordingRunner{}
		timerCh := make(chan time.Time, 1)
		impl, err := newBatchDelivery(batchDeliveryDeps{
			Owner: "default-owner", Done: done, Elect: elector, Runner: runner,
			Timer: func(time.Duration) <-chan time.Time { return timerCh },
		})
		require.NoError(t, err)
		return impl, elector, runner, timerCh
	}
	inv := BatchInvocation{
		SessionID:        "sess-1",
		Delivery:         batchDeliveryChoice{Mode: batchDeliverySync, Window: "1h"},
		BackgroundEscape: batchEscapeAuto,
	}

	// Twin 1: completion within the window.
	done := newGatedDoneSource()
	envelope := fantasy.NewTextResponse("window-done")
	done.arm(envelope)
	impl, elector, runner, _ := buildDelivery(done)
	got, err := impl.DeliverBatch(context.Background(), "root-1", inv)
	require.NoError(t, err)
	require.Equal(t, envelope, got, "an in-window completion is returned unchanged")
	require.Zero(t, elector.startedCount(), "no started election when the completion arrives in window")
	require.Zero(t, runner.stopCount())
	require.Zero(t, runner.submitCount())

	// Twin 2: held completion past the window.
	held := newGatedDoneSource()
	impl, elector, runner, timerCh := buildDelivery(held)
	type delivered struct {
		resp fantasy.ToolResponse
		err  error
	}
	respCh := make(chan delivered, 1)
	go func() {
		resp, err := impl.DeliverBatch(context.Background(), "root-1", inv)
		respCh <- delivered{resp, err}
	}()
	timerCh <- time.Now()
	late := <-respCh
	require.NoError(t, late.err)
	require.Equal(t,
		fmt.Sprintf("batch root %s %s", jobbatch.ID("root-1"), batchStartedResponseSuffix),
		late.resp.Content,
		"window elapse elects started and returns the started text")
	require.Equal(t, 1, elector.startedCount())
	held.arm(fantasy.NewTextResponse("late-done"))
	require.Eventually(t, func() bool { return held.awaitCount() == 1 },
		5*time.Second, time.Millisecond, "exactly one AwaitDone waiter was launched")
	held.release()
	require.Zero(t, runner.stopCount(), "no timer-induced Stop")
	require.Zero(t, runner.submitCount())
}
