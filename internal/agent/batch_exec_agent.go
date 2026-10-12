package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// batchAgentRunRequest identifies one incarnation attempt of the leaf.
type batchAgentRunRequest struct {
	NodeID     jobbatch.ID
	Token      uint64
	SessionID  string
	ToolCallID string
	Input      json.RawMessage
}

// batchAgentHost is the per-incarnation control surface handed to the run
// factory. Ask parks the ask_question tool call on a held question until an
// injected answer at the same depth arrives; the tool turns it into a
// successful question-shaped response, keeping the node live meanwhile.
type batchAgentHost struct {
	Ask func(ctx context.Context, depth int, question string) (json.RawMessage, error)
}

// batchAgentTurnResult is one turn loop's outcome.
type batchAgentTurnResult struct {
	Response fantasy.ToolResponse
	// Declared is an optional task_outcome declaration (design 9.3). Open
	// decision, smallest choice: it may only reclassify a clean closure, is
	// scoped to this incarnation and never overwrites a terminal node.
	Declared jobbatch.Outcome
}

// batchAgentTurn is one launched agent turn loop.
type batchAgentTurn interface {
	Done() <-chan struct{}
	// Exit closes when the driver goroutine fully exited, including any late
	// kill work; it may lag Done.
	Exit() <-chan struct{}
	Result() batchAgentTurnResult // Valid after Done.
	Err() error                   // Non-nil: the loop failed or was cancelled.
	// Stop acknowledges the stop request. The acknowledgement never settles
	// the node; only confirmed termination does.
	Stop(ctx context.Context) error
}

// batchAgentRunFactory launches the agent turn loop (the driver).
type batchAgentRunFactory interface {
	StartAgentTurn(ctx context.Context, req batchAgentRunRequest, host batchAgentHost) (batchAgentTurn, error)
}

// batchAgentDescendantReader is the injected live-root seam (R10a): it
// reports whether the incarnation still has live descendants, with a
// completeness flag; an incomplete read keeps the scope open.
type batchAgentDescendantReader interface {
	DescendantsLive(ctx context.Context, req batchAgentRunRequest) (live bool, complete bool)
}

// batchAgentDeps carries every injected dependency; no global state.
type batchAgentDeps struct {
	Runs        batchAgentRunFactory
	Descendants batchAgentDescendantReader
	// Poll is the descendant re-read interval while the scope stays open.
	// Zero means a single read after the turn ends.
	Poll time.Duration
}

// batchAgentFamily implements the engine executor contract for the agent
// leaf kind. It is inert until registered with a batchExecutor by R11.
type batchAgentFamily struct{ deps batchAgentDeps }

func newBatchAgentFamily(deps batchAgentDeps) batchLeafFamily {
	return batchAgentFamily{deps: deps}
}

var _ batchLeafFamily = batchAgentFamily{}

func (f batchAgentFamily) start(ctx context.Context, _ batchLeafEnv, l jobbatch.Launch, p batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req := batchAgentRunRequest{
		NodeID:     l.ID,
		Token:      l.Token,
		SessionID:  p.SessionID,
		ToolCallID: p.ToolCallID,
		Input:      p.Input,
	}
	e := &batchAgentExecution{
		deps:        f.deps,
		req:         req,
		scope:       newBatchAgentScope(),
		driverDone:  make(chan struct{}),
		watcherDone: make(chan struct{}),
		finished:    make(chan struct{}),
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.cancel = cancel
	turn, err := e.launch(runCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	e.mu.Lock()
	e.turn = turn
	e.mu.Unlock()
	e.internal.Add(2)
	go func() {
		defer e.internal.Done()
		e.watch(runCtx, turn, report)
	}()
	go func() {
		defer e.internal.Done()
		<-turn.Exit()
		close(e.driverDone)
	}()
	return e, nil
}

// launch calls the run factory with panic containment; a panic means no
// execution exists.
func (e *batchAgentExecution) launch(runCtx context.Context) (turn batchAgentTurn, err error) {
	defer func() {
		if r := recover(); r != nil {
			turn, err = nil, &batchStartError{"agent_start_panic", fmt.Errorf("%v", r)}
		}
	}()
	return e.deps.Runs.StartAgentTurn(runCtx, e.req, batchAgentHost{Ask: e.ask})
}

type batchAgentExecution struct {
	deps   batchAgentDeps
	req    batchAgentRunRequest
	scope  *batchAgentScope
	cancel context.CancelFunc

	mu       sync.Mutex
	turn     batchAgentTurn
	stopped  bool
	disposed bool
	content  []byte
	result   jobbatch.Result
	reported bool

	driverDone  chan struct{} // Driver goroutine exit: late kill barrier.
	watcherDone chan struct{}
	finished    chan struct{}  // Closed once, when the scope result is fixed.
	internal    sync.WaitGroup // Adapter goroutines; zero again after Dispose.
}

var (
	_ jobbatch.Execution    = (*batchAgentExecution)(nil)
	_ jobbatch.OutputReader = (*batchAgentExecution)(nil)
	_ jobbatch.Messenger    = (*batchAgentExecution)(nil)
	_ jobbatch.Disposer     = (*batchAgentExecution)(nil)
)

// heldDepth reports the depth of the currently held question, if any.
func (e *batchAgentExecution) heldDepth() (int, bool) {
	e.scope.mu.Lock()
	defer e.scope.mu.Unlock()
	if e.scope.held == nil {
		return 0, false
	}
	return e.scope.held.depth, true
}

// ask implements the host question channel of one incarnation.
func (e *batchAgentExecution) ask(ctx context.Context, depth int, question string) (json.RawMessage, error) {
	h, err := e.scope.hold(depth)
	if err != nil {
		return nil, err
	}
	select {
	case answer := <-h.answer:
		return answer, nil
	case <-ctx.Done():
		e.scope.discardHold(h)
		return nil, ctx.Err()
	}
}

// watch runs the scope watcher: it waits for the turn to end, then for a
// confirmed scope closure, and reports exactly once.
func (e *batchAgentExecution) watch(ctx context.Context, turn batchAgentTurn, report func(jobbatch.Result)) {
	defer close(e.watcherDone)
	select {
	case <-turn.Done():
	case <-ctx.Done():
		// Stop cancelled the run context; wait for the loop itself to exit.
		<-turn.Done()
	}
	e.scope.turnEnded()
	poll := e.deps.Poll
	if poll <= 0 {
		poll = 10 * time.Millisecond
	}
	for !e.scope.closedNow() {
		// Reads survive a stop cancellation: confirmed closure is a fact,
		// not an acknowledgement.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), poll)
		live, complete := e.deps.Descendants.DescendantsLive(dctx, e.req)
		cancel()
		e.scope.descendants(live, complete)
		if e.scope.closedNow() {
			break
		}
		e.scope.waitChange(dctx)
	}
	// The scope facts are the confirmed termination; the driver's own exit
	// (late kill) may lag and is waited for by Dispose.
	if !e.scope.markClosed() {
		return // Another path closed it; report exactly once.
	}
	e.finish(batchAgentScopeResult(turn.Result(), turn.Err(), e.scope.stop()), report)
}

// finish fixes the terminal result once, retains the output and reports.
func (e *batchAgentExecution) finish(result jobbatch.Result, report func(jobbatch.Result)) {
	e.mu.Lock()
	if e.reported {
		e.mu.Unlock()
		return
	}
	e.reported = true
	e.result = result
	e.content = []byte(result.Summary)
	close(e.finished)
	e.mu.Unlock()
	report(result)
}

// Stop cancels the run context (the addressed subtree at depth), records the
// stop intent and returns the turn's acknowledgement. It never settles.
func (e *batchAgentExecution) Stop(ctx context.Context) error {
	e.mu.Lock()
	if e.disposed {
		e.mu.Unlock()
		return jobbatch.ErrReleased
	}
	first := !e.stopped
	e.stopped = true
	turn := e.turn
	e.mu.Unlock()
	if first {
		e.scope.beginStop()
		e.cancel()
	}
	if turn == nil {
		return nil
	}
	return turn.Stop(ctx)
}

// Inject delivers an answer or message into this incarnation. It is rejected
// deterministically when the node is stopping, finished or holds no matching
// question, so a message is never lost silently nor delivered to a new
// incarnation.
func (e *batchAgentExecution) Inject(_ context.Context, msg json.RawMessage) error {
	e.mu.Lock()
	if e.disposed {
		e.mu.Unlock()
		return jobbatch.ErrReleased
	}
	finished := e.reported
	stopped := e.stopped
	e.mu.Unlock()
	if finished {
		return ErrBatchAgentStaleGeneration
	}
	if stopped {
		return ErrBatchAgentStopped
	}
	var m batchAgentInjectMessage
	if err := json.Unmarshal(msg, &m); err != nil {
		return fmt.Errorf("%w: %v", ErrBatchAgentNotHeld, err)
	}
	return e.scope.deliver(m)
}

// ReadOutput returns the retained result summary as one terminal chunk.
func (e *batchAgentExecution) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	select {
	case <-e.finished:
	case <-ctx.Done():
		return jobbatch.OutputChunk{}, ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disposed {
		return jobbatch.OutputChunk{}, jobbatch.ErrReleased
	}
	next := uint64(len(e.content))
	if cursor > next {
		cursor = next
	}
	return jobbatch.OutputChunk{Data: append([]byte(nil), e.content[cursor:]...), Next: next, EOF: true}, nil
}

// Dispose waits for the late kill (driver exit) and the watcher, then
// releases the generation state: the question channel and retained output.
func (e *batchAgentExecution) Dispose(ctx context.Context) error {
	select {
	case <-e.finished:
	default:
		return jobbatch.ErrNotTerminal
	}
	// Barrier: the late kill (driver exit) precedes any release.
	select {
	case <-e.driverDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-e.watcherDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disposed {
		return nil
	}
	e.disposed = true
	e.content = nil
	return nil
}

// batchAgentScopeResult classifies the confirmed closure. A stop
// acknowledgement alone is never a settlement: cancellation is reported only
// after the scope confirmed closure.
func batchAgentScopeResult(res batchAgentTurnResult, turnErr error, stopped bool) jobbatch.Result {
	r := jobbatch.Result{}
	switch {
	case stopped && (errors.Is(turnErr, context.Canceled) || errors.Is(turnErr, context.DeadlineExceeded)):
		r.State = jobbatch.StateCancelled
	case turnErr != nil:
		r.State, r.Code = jobbatch.StateFailed, "agent_run_error"
		r.Summary = fmt.Sprintf("%v", turnErr)
	case res.Response.IsError:
		r.State, r.Code = jobbatch.StateFailed, "agent_error"
		r.Summary = res.Response.Content
	default:
		r.State = jobbatch.StateCompleted
		r.Summary = res.Response.Content
		// A non-reportable declaration is ignored, never turned into an empty
		// state.
		if declared := res.Declared; declared.State.Reportable() {
			r.State, r.Code, r.Summary = declared.State, declared.Code, declared.Summary
		}
	}
	if r.Summary == "" {
		r.Summary = res.Response.Content
	}
	r.Details = batchEnvelope(res.Response)
	return r
}
