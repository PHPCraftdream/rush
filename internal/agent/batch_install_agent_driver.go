package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// R11F (M3): coordinator seams of the R7c agent executor family. The M2
// install left batchAgentDeps zero; the flagged install wires
// CoordinatorBatchAgentDeps, whose factory resolves the ENCLOSED
// invocation's pinned "agent" leaf (hooks/restricted retained) through the
// one ingress and runs it as the incarnation's turn loop.

// errBatchCoordinatorNotBound: the flag-on wiring (BindCoordinatorBatchRuntime)
// has not run — the agent family refuses instead of dispatching raw.
var errBatchCoordinatorNotBound = errors.New("batch agent family: no coordinator batch binding (flag-on wiring missing)")

// batchCoordinatorAgentRuns is the R7c run factory over the process binding.
type batchCoordinatorAgentRuns struct{}

// StartAgentTurn launches one incarnation: the pinned delegation leaf runs
// to completion on its own goroutine. R11F round-2 (P1-3): dispatch is by
// the RECORDED tool name (req.Tool) — an "agentic_fetch" payload resolves
// its own pinned leaf and names the call agentic_fetch; an empty name fails
// closed.
//
// R11F round-2 (P1-5a), host.Ask wiring decision — DIRECT IS CORRECT, not a
// discard: the engine-side host.Ask is batchAgentExecution.ask, whose hold
// is answered only by the engine's Inject (batchAgentExecution.Inject →
// scope.deliver). The R7c delegation leaf deliberately keeps its OWN
// question machinery (question_stop.go / delegation_question.go): a child
// asking ask_question FINISHES its turn with a question-shaped block, the
// scope closes, and the parent's answer arrives as a RESUME — a fresh
// generation. Wiring host.Ask into the leaf's turn would park the incarnation
// on a hold no one answers (the leaf no longer runs a turn), i.e. the exact
// deadlock R7c removed. The hold channel stays reachable through Inject for
// operators and tests.
func (batchCoordinatorAgentRuns) StartAgentTurn(ctx context.Context, req batchAgentRunRequest, host batchAgentHost) (batchAgentTurn, error) {
	_ = host // See P1-5a note above: direct is correct; the hold is served via Inject.
	bind := processCoordinatorBatchBinding()
	if bind == nil {
		return nil, errBatchCoordinatorNotBound
	}
	if req.RegistryID == "" {
		return nil, fmt.Errorf("%w: batch agent incarnation", errBatchMissingRegistryID)
	}
	if req.Tool == "" {
		// P1-3: fail closed rather than silently dispatching "agent".
		return nil, fmt.Errorf("%w: batch agent incarnation without a recorded tool name", errBatchUnknownTool)
	}
	leaf, err := bind.ingress.ResolveLeaf(req.RegistryID, req.Tool)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	runCtx = context.WithValue(runCtx, tools.SessionIDContextKey, req.SessionID)
	runCtx = context.WithValue(runCtx, tools.MessageIDContextKey, req.MessageID)
	// P1-4: stamp the TRUSTED enclosing invocation so the CLI bridge binds
	// to THIS invocation's pinned registry (never the latest one).
	runCtx = withBatchBridgeInvocation(runCtx, batchBridgeInvocation{RegistryID: req.RegistryID, MessageID: req.MessageID})
	call := fantasy.ToolCall{ID: req.ToolCallID, Name: req.Tool, Input: string(req.Input)}
	turn := &batchCoordinatorAgentTurn{cancel: cancel, done: make(chan struct{}), exit: make(chan struct{})}
	go func() {
		defer close(turn.exit)
		defer close(turn.done)
		resp, runErr := func() (resp fantasy.ToolResponse, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("batch agent leaf panic: %v", r)
				}
			}()
			return leaf.Run(runCtx, call)
		}()
		turn.mu.Lock()
		turn.resp, turn.err = resp, runErr
		turn.mu.Unlock()
		cancel()
	}()
	return turn, nil
}

// batchCoordinatorAgentTurn is one launched incarnation.
type batchCoordinatorAgentTurn struct {
	cancel      context.CancelFunc
	done, exit  chan struct{}
	mu          sync.Mutex
	resp        fantasy.ToolResponse
	err         error
	stopPending bool
}

func (t *batchCoordinatorAgentTurn) Done() <-chan struct{} { return t.done }
func (t *batchCoordinatorAgentTurn) Exit() <-chan struct{} { return t.exit }

func (t *batchCoordinatorAgentTurn) Result() batchAgentTurnResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	return batchAgentTurnResult{Response: t.resp}
}

func (t *batchCoordinatorAgentTurn) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Stop acknowledges the stop request by cancelling the run context; the
// scope watcher still settles only on confirmed closure.
func (t *batchCoordinatorAgentTurn) Stop(context.Context) error {
	t.mu.Lock()
	first := !t.stopPending
	t.stopPending = true
	t.mu.Unlock()
	if first {
		t.cancel()
	}
	return nil
}

// CoordinatorBatchAgentDeps builds the flag-on agent-family seams for the
// M1 install (BatchInstallDeps.AgentDeps). Before the process binding runs
// the family fails closed with errBatchCoordinatorNotBound.
// CoordinatorBatchAgentDeps builds the flag-on agent-family seams WITHOUT
// the store-backed descendant reader: DescendantsLive then reports an
// INCOMPLETE read (false, false) — the scope stays open, never an optimistic
// closure. Flagged installs use CoordinatorBatchAgentDepsWithLive (P1-5b).
func CoordinatorBatchAgentDeps() BatchAgentDeps {
	return batchAgentDeps{
		Runs:        batchCoordinatorAgentRuns{},
		Descendants: batchCoordinatorDescendants{},
	}
}
