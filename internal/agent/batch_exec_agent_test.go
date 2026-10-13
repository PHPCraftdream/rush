package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// ---- fakes -------------------------------------------------------------

type batchAgentTestTurn struct {
	done    chan struct{}
	exit    chan struct{}
	resMu   sync.Mutex
	res     batchAgentTurnResult
	err     error
	stopped chan struct{}
	stopErr error
	once    sync.Once
}

func newBatchAgentTestTurn(res batchAgentTurnResult, err error) *batchAgentTestTurn {
	return &batchAgentTestTurn{
		done: make(chan struct{}), exit: make(chan struct{}),
		res: res, err: err, stopped: make(chan struct{}),
	}
}

func (t *batchAgentTestTurn) finish() {
	t.once.Do(func() { close(t.done) })
}

// exitNow ends the driver exit barrier; it may lag finish.
func (t *batchAgentTestTurn) exitNow() {
	t.once.Do(func() { close(t.done) })
	select {
	case <-t.exit:
	default:
		close(t.exit)
	}
}

func (t *batchAgentTestTurn) setRes(res batchAgentTurnResult, err error) {
	t.resMu.Lock()
	t.res, t.err = res, err
	t.resMu.Unlock()
}

func (t *batchAgentTestTurn) Done() <-chan struct{} {
	return t.done
}

func (t *batchAgentTestTurn) Exit() <-chan struct{} {
	return t.exit
}

func (t *batchAgentTestTurn) Result() batchAgentTurnResult {
	t.resMu.Lock()
	defer t.resMu.Unlock()
	return t.res
}

func (t *batchAgentTestTurn) Err() error {
	t.resMu.Lock()
	defer t.resMu.Unlock()
	return t.err
}

func (t *batchAgentTestTurn) Stop(context.Context) error {
	select {
	case <-t.stopped:
	default:
		close(t.stopped)
	}
	return t.stopErr
}

type batchAgentTestFactory struct {
	turn    *batchAgentTestTurn
	turnErr error
	host    batchAgentHost
	req     batchAgentRunRequest
	ctx     context.Context // The run context the factory received.
	started chan struct{}
	panics  bool
	useTurn func(*batchAgentTestTurn, batchAgentHost)
}

func newBatchAgentTestFactory(turn *batchAgentTestTurn) *batchAgentTestFactory {
	return &batchAgentTestFactory{turn: turn, started: make(chan struct{}, 1)}
}

func (f *batchAgentTestFactory) StartAgentTurn(ctx context.Context, req batchAgentRunRequest, host batchAgentHost) (batchAgentTurn, error) {
	if f.panics {
		panic("factory boom")
	}
	f.host, f.req, f.ctx = host, req, ctx
	select {
	case f.started <- struct{}{}:
	default:
	}
	if f.turnErr != nil {
		return nil, f.turnErr
	}
	turn := f.turn
	if use := f.useTurn; use != nil {
		use(turn, host)
	}
	return turn, nil
}

// askAndShape mimics the ask_question tool: it asks through the host and
// only finishes the turn once the answer arrived.
func askAndShape(turn *batchAgentTestTurn, host batchAgentHost, depth int, question string) {
	go func() {
		answer, err := host.Ask(context.Background(), depth, question)
		if err != nil {
			turn.setRes(batchAgentTurnResult{Response: fantasy.NewTextErrorResponse(err.Error())}, nil)
		} else {
			turn.setRes(batchAgentTurnResult{Response: fantasy.NewTextResponse(string(answer))}, nil)
		}
		turn.finish()
		turn.exitNow()
	}()
}

type batchAgentTestReader struct {
	mu       sync.Mutex
	live     bool
	complete bool
	reads    int
}

func newBatchAgentTestReader(live, complete bool) *batchAgentTestReader {
	return &batchAgentTestReader{live: live, complete: complete}
}

func (r *batchAgentTestReader) set(live, complete bool) {
	r.mu.Lock()
	r.live, r.complete = live, complete
	r.mu.Unlock()
}

func (r *batchAgentTestReader) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

func (r *batchAgentTestReader) DescendantsLive(context.Context, batchAgentRunRequest) (bool, bool) {
	r.mu.Lock()
	r.reads++
	live, complete := r.live, r.complete
	r.mu.Unlock()
	return live, complete
}

func batchAgentTestRun(t *testing.T, deps batchAgentDeps, factory *batchAgentTestFactory) (*batchAgentExecution, *batchAgentTestFactory, *batchAgentTestReader, chan jobbatch.Result) {
	t.Helper()
	reader := newBatchAgentTestReader(false, false)
	deps.Runs = factory
	deps.Descendants = reader
	if deps.Poll == 0 {
		deps.Poll = time.Millisecond
	}
	e := newBatchExecutor(batchLeafEnv{}, map[string]batchLeafFamily{batchKindAgent: newBatchAgentFamily(deps)})
	payload, err := json.Marshal(batchLeafPayload{Tool: "agent", ToolCallID: "call", SessionID: t.Name(), Input: json.RawMessage(`{}`)})
	require.NoError(t, err)
	reports := make(chan jobbatch.Result, 4)
	exec, err := e.Start(context.Background(), jobbatch.Launch{ID: "root.1", Token: 1, Task: jobbatch.TaskSpec{Kind: batchKindAgent, Payload: payload}}, func(r jobbatch.Result) { reports <- r })
	require.NoError(t, err)
	select {
	case <-factory.started:
	case <-time.After(2 * time.Second):
		t.Fatal("run factory never started")
	}
	return exec.(*batchAgentExecution), factory, reader, reports
}

func batchAgentTestNoReport(t *testing.T, reports chan jobbatch.Result, why string) {
	t.Helper()
	select {
	case r := <-reports:
		t.Fatalf("scope reported while %s: %+v", why, r)
	case <-time.After(80 * time.Millisecond):
	}
}

func batchAgentTestReport(t *testing.T, reports chan jobbatch.Result) jobbatch.Result {
	t.Helper()
	select {
	case r := <-reports:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("termination report missing")
		return jobbatch.Result{}
	}
}

func batchAgentTestPayload(t *testing.T, session string) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(batchLeafPayload{Tool: "agent", ToolCallID: "call", SessionID: session, Input: json.RawMessage(`{}`)})
	require.NoError(t, err)
	return payload
}

// ---- tests -------------------------------------------------------------

// RC1/RC13: the scope reports only after the turn ended AND descendants are
// confirmed terminal, exactly once.
func TestBatchAgentScopeWaitsForTurnAndDescendants(t *testing.T) {
	turn := newBatchAgentTestTurn(batchAgentTurnResult{Response: fantasy.NewTextResponse("done")}, nil)
	e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turn))

	// Turn running, descendants terminal: still no report.
	reader.set(false, true)
	batchAgentTestNoReport(t, reports, "the turn loop runs")

	// Turn ended, descendants live: the node stays live and Running.
	turn.finish()
	turn.exitNow()
	reader.set(true, true)
	batchAgentTestNoReport(t, reports, "descendants are live")

	// An unknown-but-incomplete read is still live, never falsely finished.
	reader.set(false, false)
	batchAgentTestNoReport(t, reports, "the read is incomplete")

	// Confirmed closure: exactly one Completed report.
	reader.set(false, true)
	r := batchAgentTestReport(t, reports)
	require.Equal(t, jobbatch.StateCompleted, r.State)
	select {
	case r := <-reports:
		t.Fatalf("duplicate report: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, jobbatch.Disposer(e).Dispose(context.Background()))
}

// RC3: an incomplete read is treated as live, never as closed.
func TestBatchAgentIncompleteReadKeepsScopeOpen(t *testing.T) {
	turn := newBatchAgentTestTurn(batchAgentTurnResult{Response: fantasy.NewTextResponse("done")}, nil)
	e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turn))
	reader.set(true, false) // Incomplete read: unknown, conservative.
	turn.finish()
	turn.exitNow()
	batchAgentTestNoReport(t, reports, "the descendant read is incomplete")
	reader.set(false, false)
	batchAgentTestNoReport(t, reports, "the read is unknown")
	reader.set(false, true)
	r := batchAgentTestReport(t, reports)
	require.Equal(t, jobbatch.StateCompleted, r.State)
	require.NoError(t, jobbatch.Disposer(e).Dispose(context.Background()))
}

// RC10: a held question keeps the node live, with and without other work.
func TestBatchAgentHeldQuestionKeepsNodeLive(t *testing.T) {
	for _, otherWork := range []bool{false, true} {
		t.Run(fmt.Sprintf("other_work_%v", otherWork), func(t *testing.T) {
			turn := newBatchAgentTestTurn(batchAgentTurnResult{}, nil)
			factory := newBatchAgentTestFactory(turn)
			factory.useTurn = func(t2 *batchAgentTestTurn, host batchAgentHost) {
				askAndShape(t2, host, 2, "why?")
			}
			e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, factory)
			require.Eventually(t, func() bool {
				_, held := e.heldDepth()
				return held
			}, 2*time.Second, time.Millisecond)
			if otherWork {
				reader.set(true, true)
				batchAgentTestNoReport(t, reports, "other work is live")
			}
			// Even with descendants terminal, the held question keeps it open.
			reader.set(false, true)
			batchAgentTestNoReport(t, reports, "a question is held")
			// The answer is injected at the held depth; the question-shaped
			// response carries it and the node settles Completed.
			require.NoError(t, jobbatch.Messenger(e).Inject(context.Background(), json.RawMessage(`{"depth":2,"text":"because"}`)))
			reader.set(false, true)
			r := batchAgentTestReport(t, reports)
			require.Equal(t, jobbatch.StateCompleted, r.State)
			require.Contains(t, r.Summary, "because")
		})
	}
}

// RC10 backstop: even with the turn ended, a held question keeps the scope
// open until the answer is injected.
func TestBatchAgentHeldQuestionAfterTurnEnded(t *testing.T) {
	turn := newBatchAgentTestTurn(batchAgentTurnResult{}, nil)
	e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turn))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.ask(context.Background(), 4, "why?")
	}()
	require.Eventually(t, func() bool {
		depth, held := e.heldDepth()
		return held && depth == 4
	}, 2*time.Second, time.Millisecond)
	turn.finish()
	turn.exitNow()
	reader.set(false, true)
	batchAgentTestNoReport(t, reports, "a question is held after the turn ended")
	require.NoError(t, jobbatch.Messenger(e).Inject(context.Background(), json.RawMessage(`{"depth":4,"text":"because"}`)))
	r := batchAgentTestReport(t, reports)
	require.Equal(t, jobbatch.StateCompleted, r.State)
	// The report can arrive before the asker goroutine is scheduled again:
	// wait for its exit instead of sampling the channel once.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("asker goroutine did not exit after the answer")
	}
}

// RC7: an answer reaches exactly its addressed depth, never another one.
func TestBatchAgentInjectRoutesByDepth(t *testing.T) {
	turn := newBatchAgentTestTurn(batchAgentTurnResult{}, nil)
	factory := newBatchAgentTestFactory(turn)
	factory.useTurn = func(t2 *batchAgentTestTurn, host batchAgentHost) {
		askAndShape(t2, host, 3, "why?")
	}
	e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, factory)
	require.Eventually(t, func() bool {
		depth, held := e.heldDepth()
		return held && depth == 3
	}, 2*time.Second, time.Millisecond)
	inj := jobbatch.Messenger(e)
	require.ErrorIs(t, inj.Inject(context.Background(), json.RawMessage(`{"depth":9}`)), ErrBatchAgentNotHeld)
	require.ErrorIs(t, inj.Inject(context.Background(), json.RawMessage(`{"depth":1}`)), ErrBatchAgentNotHeld)
	require.NoError(t, inj.Inject(context.Background(), json.RawMessage(`{"depth":3,"text":"answer"}`)))
	reader.set(false, true)
	r := batchAgentTestReport(t, reports)
	require.Equal(t, jobbatch.StateCompleted, r.State)
	require.Contains(t, r.Summary, "answer")
}

// RC4: Inject and Stop race; both orders hold over 200 iterations: Stop
// succeeds, Inject either succeeds into the live generation or is rejected,
// and a message is never delivered after terminality.
func TestBatchAgentInjectVsStopRace(t *testing.T) {
	for i := 0; i < 200; i++ {
		turn := newBatchAgentTestTurn(batchAgentTurnResult{Response: fantasy.NewTextResponse("done")}, context.Canceled)
		e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turn))
		var wg sync.WaitGroup
		stopErr := make(chan error, 1)
		injectErr := make(chan error, 1)
		wg.Add(1)
		go func() { defer wg.Done(); stopErr <- e.Stop(context.Background()) }()
		wg.Add(1)
		go func(delay time.Duration) {
			defer wg.Done()
			time.Sleep(delay)
			injectErr <- jobbatch.Messenger(e).Inject(context.Background(), json.RawMessage(`{"depth":1}`))
		}(time.Duration(i%3) * 25 * time.Microsecond)
		wg.Wait()
		require.NoError(t, <-stopErr)
		err := <-injectErr
		require.True(t, err == nil || errors.Is(err, ErrBatchAgentStopped) || errors.Is(err, ErrBatchAgentNotHeld),
			"iteration %d: unexpected inject error %v", i, err)
		turn.finish()
		turn.exitNow()
		reader.set(false, true)
		r := batchAgentTestReport(t, reports)
		require.Equal(t, jobbatch.StateCancelled, r.State)
		require.ErrorIs(t, jobbatch.Messenger(e).Inject(context.Background(), json.RawMessage(`{"depth":1}`)), ErrBatchAgentStaleGeneration)
	}
}

// RC4 (deterministic order): a message is never delivered after Stop.
func TestBatchAgentInjectRejectedAfterStop(t *testing.T) {
	turn := newBatchAgentTestTurn(batchAgentTurnResult{Response: fantasy.NewTextResponse("x")}, context.Canceled)
	e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turn))
	require.NoError(t, e.Stop(context.Background()))
	require.ErrorIs(t, jobbatch.Messenger(e).Inject(context.Background(), json.RawMessage(`{"depth":1}`)), ErrBatchAgentStopped)
	turn.finish()
	turn.exitNow()
	reader.set(false, true)
	r := batchAgentTestReport(t, reports)
	require.Equal(t, jobbatch.StateCancelled, r.State)
}

// RC5: a terminal incarnation rejects Inject as stale; a message can never
// reach a new incarnation through the old handle.
func TestBatchAgentInjectStaleGeneration(t *testing.T) {
	turn := newBatchAgentTestTurn(batchAgentTurnResult{Response: fantasy.NewTextResponse("done")}, nil)
	e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turn))
	turn.finish()
	turn.exitNow()
	reader.set(false, true)
	require.Equal(t, jobbatch.StateCompleted, batchAgentTestReport(t, reports).State)
	require.ErrorIs(t, jobbatch.Messenger(e).Inject(context.Background(), json.RawMessage(`{"depth":1}`)), ErrBatchAgentStaleGeneration)
}

// RC8: Stop at depth touches exactly the addressed subtree; a sibling leaf
// from the same family runs to completion.
func TestBatchAgentStopAtDepthLeavesSiblingsUntouched(t *testing.T) {
	turnA := newBatchAgentTestTurn(batchAgentTurnResult{Response: fantasy.NewTextResponse("a")}, context.Canceled)
	turnB := newBatchAgentTestTurn(batchAgentTurnResult{Response: fantasy.NewTextResponse("b")}, nil)
	eA, _, reader, reportsA := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turnA))

	// Sibling B shares only the descendant reader.
	readerB := newBatchAgentTestReader(false, false)
	depsB := batchAgentDeps{Runs: &batchAgentStaticFactory{turn: turnB}, Descendants: readerB, Poll: time.Millisecond}
	eB := newBatchExecutor(batchLeafEnv{}, map[string]batchLeafFamily{batchKindAgent: newBatchAgentFamily(depsB)})
	reportsB := make(chan jobbatch.Result, 2)
	_, err := eB.Start(context.Background(), jobbatch.Launch{ID: "root.2", Token: 1, Task: jobbatch.TaskSpec{Kind: batchKindAgent, Payload: batchAgentTestPayload(t, "sibling")}}, func(r jobbatch.Result) { reportsB <- r })
	require.NoError(t, err)

	require.NoError(t, eA.Stop(context.Background()))
	turnA.finish()
	turnA.exitNow()
	turnB.finish()
	turnB.exitNow()
	reader.set(false, true)
	readerB.set(false, true)
	require.Equal(t, jobbatch.StateCancelled, batchAgentTestReport(t, reportsA).State)
	rb := batchAgentTestReport(t, reportsB)
	require.Equal(t, jobbatch.StateCompleted, rb.State)
	require.Equal(t, "b", rb.Summary)
}

// RC6: the stop acknowledgement never settles the node.
func TestBatchAgentStopAckDoesNotSettle(t *testing.T) {
	turn := newBatchAgentTestTurn(batchAgentTurnResult{Response: fantasy.NewTextResponse("x")}, context.Canceled)
	e, factory, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turn))
	// The turn already ended; the descendants are still live.
	reader.set(true, true)
	turn.finish()
	turn.exitNow()
	batchAgentTestNoReport(t, reports, "descendants are live before the stop")
	require.NoError(t, e.Stop(context.Background()))
	// The stop reached the incarnation's run context, but the acknowledgement
	// alone never settles the node.
	require.Eventually(t, func() bool { return factory.ctx.Err() != nil }, 2*time.Second, time.Millisecond)
	batchAgentTestNoReport(t, reports, "descendants are still live after the ack")
	reader.set(false, true)
	r := batchAgentTestReport(t, reports)
	require.Equal(t, jobbatch.StateCancelled, r.State)
}

// RC8b: Dispose waits for the late kill (driver exit) before releasing.
func TestBatchAgentDisposeWaitsForLateKill(t *testing.T) {
	turn := newBatchAgentTestTurn(batchAgentTurnResult{Response: fantasy.NewTextResponse("x")}, context.Canceled)
	lateKill := make(chan struct{})
	factory := newBatchAgentTestFactory(turn)
	factory.useTurn = func(t2 *batchAgentTestTurn, _ batchAgentHost) {
		go func() {
			<-lateKill
			close(t2.exit)
		}()
	}
	e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, factory)
	require.NoError(t, e.Stop(context.Background()))
	turn.finish() // The turn loop ended; the kill work lags on exit.
	reader.set(false, true)
	require.Equal(t, jobbatch.StateCancelled, batchAgentTestReport(t, reports).State)

	dctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, jobbatch.Disposer(e).Dispose(dctx), context.DeadlineExceeded)
	close(lateKill)
	require.NoError(t, jobbatch.Disposer(e).Dispose(context.Background()))
	_, err := jobbatch.OutputReader(e).ReadOutput(context.Background(), 0)
	require.ErrorIs(t, err, jobbatch.ErrReleased)
}

// RC9: Dispose releases the generation state; no goroutine leaks.
func TestBatchAgentDisposeGenerationCleanup(t *testing.T) {
	turn := newBatchAgentTestTurn(batchAgentTurnResult{}, nil)
	factory := newBatchAgentTestFactory(turn)
	var askers sync.WaitGroup
	factory.useTurn = func(t2 *batchAgentTestTurn, host batchAgentHost) {
		askers.Add(1)
		go func() {
			defer askers.Done()
			answer, err := host.Ask(context.Background(), 1, "why?")
			if err != nil {
				t2.setRes(batchAgentTurnResult{Response: fantasy.NewTextErrorResponse(err.Error())}, nil)
			} else {
				t2.setRes(batchAgentTurnResult{Response: fantasy.NewTextResponse(string(answer))}, nil)
			}
			t2.finish()
			t2.exitNow()
		}()
	}
	e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, factory)
	exec := e
	require.Eventually(t, func() bool {
		_, held := exec.heldDepth()
		return held
	}, 2*time.Second, time.Millisecond)
	require.NoError(t, jobbatch.Messenger(e).Inject(context.Background(), json.RawMessage(`{"depth":1,"text":"because"}`)))
	reader.set(false, true)
	require.Equal(t, jobbatch.StateCompleted, batchAgentTestReport(t, reports).State)
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()
	require.NoError(t, jobbatch.Disposer(e).Dispose(dctx))

	// Generation state is released: both internal goroutines exited.
	select {
	case <-exec.driverDone:
	default:
		t.Fatal("driver exit barrier not released on Dispose")
	}
	select {
	case <-exec.watcherDone:
	default:
		t.Fatal("watcher not released on Dispose")
	}
	done := make(chan struct{})
	go func() { askers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("question asker goroutine leaked")
	}
	_, err := jobbatch.OutputReader(e).ReadOutput(context.Background(), 0)
	require.ErrorIs(t, err, jobbatch.ErrReleased)
	// Explicit counter: every adapter goroutine has exited after Dispose.
	released := make(chan struct{})
	go func() { exec.internal.Wait(); close(released) }()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("adapter goroutine leaked past Dispose")
	}
}

// RC12: a panic in the run factory is contained at Start.
func TestBatchAgentRunFactoryPanicContained(t *testing.T) {
	reader := newBatchAgentTestReader(false, false)
	e := newBatchExecutor(batchLeafEnv{}, map[string]batchLeafFamily{batchKindAgent: newBatchAgentFamily(batchAgentDeps{Runs: &batchAgentTestFactory{panics: true}, Descendants: reader})})
	_, startErr := e.Start(context.Background(), jobbatch.Launch{ID: "n", Task: jobbatch.TaskSpec{Kind: batchKindAgent, Payload: batchAgentTestPayload(t, "s")}}, func(jobbatch.Result) {})
	var se *batchStartError
	require.ErrorAs(t, startErr, &se)
	require.Equal(t, "agent_start_panic", se.Code())
}

// RC13b: denied and failed agent runs map to Failed with the evidence.
func TestBatchAgentDeniedOrErrorRunsFail(t *testing.T) {
	t.Run("run_error", func(t *testing.T) {
		turn := newBatchAgentTestTurn(batchAgentTurnResult{Response: fantasy.NewTextResponse("partial")}, errors.New("provider down"))
		e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turn))
		turn.finish()
		turn.exitNow()
		reader.set(false, true)
		r := batchAgentTestReport(t, reports)
		require.Equal(t, jobbatch.StateFailed, r.State)
		require.Equal(t, "agent_run_error", r.Code)
		require.Contains(t, r.Summary, "provider down")
		require.NoError(t, jobbatch.Disposer(e).Dispose(context.Background()))
	})
	t.Run("is_error", func(t *testing.T) {
		turn := newBatchAgentTestTurn(batchAgentTurnResult{Response: fantasy.NewTextErrorResponse("denied by permission")}, nil)
		e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turn))
		turn.finish()
		turn.exitNow()
		reader.set(false, true)
		r := batchAgentTestReport(t, reports)
		require.Equal(t, jobbatch.StateFailed, r.State)
		require.Equal(t, "agent_error", r.Code)
		require.Contains(t, r.Summary, "denied")
		require.NoError(t, jobbatch.Disposer(e).Dispose(context.Background()))
	})
}

// RC15: one terminal report per node even when the reader flickers and a
// stop interleaves.
func TestBatchAgentOneTerminalReportPerNode(t *testing.T) {
	turn := newBatchAgentTestTurn(batchAgentTurnResult{Response: fantasy.NewTextResponse("done")}, nil)
	e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turn))
	reader.set(true, true)
	reader.set(false, true)
	reader.set(true, true)
	turn.finish()
	turn.exitNow()
	require.NoError(t, e.Stop(context.Background()))
	reader.set(false, true)
	r := batchAgentTestReport(t, reports)
	require.Equal(t, jobbatch.StateCompleted, r.State)
	select {
	case r := <-reports:
		t.Fatalf("second report: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, jobbatch.Disposer(e).Dispose(context.Background()))
}

// task_outcome: a declared outcome may reclassify a clean closure, is scoped
// to the incarnation and cannot overwrite a terminal node.
func TestBatchAgentDeclaredOutcome(t *testing.T) {
	turn := &batchAgentTestTurn{
		done: make(chan struct{}), exit: make(chan struct{}), stopped: make(chan struct{}),
		res: batchAgentTurnResult{
			Response: fantasy.NewTextResponse("ok"),
			Declared: jobbatch.Outcome{State: jobbatch.StateFailed, Code: "task_outcome", Summary: "declared failure"},
		},
	}
	e, _, reader, reports := batchAgentTestRun(t, batchAgentDeps{}, newBatchAgentTestFactory(turn))
	turn.finish()
	turn.exitNow()
	reader.set(false, true)
	r := batchAgentTestReport(t, reports)
	require.Equal(t, jobbatch.StateFailed, r.State)
	require.Equal(t, "task_outcome", r.Code)
	// Terminal immutability: the old handle rejects further messages.
	require.ErrorIs(t, jobbatch.Messenger(e).Inject(context.Background(), json.RawMessage(`{"depth":1}`)), ErrBatchAgentStaleGeneration)
	require.NoError(t, jobbatch.Disposer(e).Dispose(context.Background()))
}

// A declaration whose state is not a reportable terminal state is ignored: the
// clean closure stays Completed, it never becomes an empty state.
func TestBatchAgentScopeResultIgnoresUnreportableDeclaration(t *testing.T) {
	for _, state := range []jobbatch.State{jobbatch.StatePending, jobbatch.StateRunning, jobbatch.StateSkipped, "bogus"} {
		r := batchAgentScopeResult(batchAgentTurnResult{
			Response: fantasy.NewTextResponse("ok"),
			Declared: jobbatch.Outcome{State: state, Code: "task_outcome", Summary: "declared"},
		}, nil, false)
		require.Equal(t, jobbatch.StateCompleted, r.State, string(state))
		require.Equal(t, "ok", r.Summary, string(state))
		require.Empty(t, r.Code, string(state))
	}
}

// RC4a/RC7 backstop: the scope itself refuses delivery into a stopped or
// closed incarnation and clears the hold exactly once.
func TestBatchScopeDeliverRejectsStopped(t *testing.T) {
	s := newBatchAgentScope()
	_, err := s.hold(1)
	require.NoError(t, err)
	s.beginStop()
	require.ErrorIs(t, s.deliver(batchAgentInjectMessage{Depth: 1}), ErrBatchAgentStopped)

	s2 := newBatchAgentScope()
	_, err = s2.hold(2)
	require.NoError(t, err)
	require.NoError(t, s2.deliver(batchAgentInjectMessage{Depth: 2}))
	require.ErrorIs(t, s2.deliver(batchAgentInjectMessage{Depth: 2}), ErrBatchAgentNotHeld)
	s2.beginStop()
	require.ErrorIs(t, s2.deliver(batchAgentInjectMessage{Depth: 2}), ErrBatchAgentStopped)
}

type batchAgentStaticFactory struct{ turn batchAgentTurn }

func (f *batchAgentStaticFactory) StartAgentTurn(context.Context, batchAgentRunRequest, batchAgentHost) (batchAgentTurn, error) {
	return f.turn, nil
}
