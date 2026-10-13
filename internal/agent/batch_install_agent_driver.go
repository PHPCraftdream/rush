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
// to completion on its own goroutine. The host's Ask channel is unused —
// the delegation leaf keeps its own question machinery (question_stop.go).
func (batchCoordinatorAgentRuns) StartAgentTurn(ctx context.Context, req batchAgentRunRequest, host batchAgentHost) (batchAgentTurn, error) {
	_ = host
	bind := processCoordinatorBatchBinding()
	if bind == nil {
		return nil, errBatchCoordinatorNotBound
	}
	if req.RegistryID == "" {
		return nil, fmt.Errorf("%w: batch agent incarnation", errBatchMissingRegistryID)
	}
	leaf, err := bind.ingress.ResolveLeaf(req.RegistryID, AgentToolName)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	runCtx = context.WithValue(runCtx, tools.SessionIDContextKey, req.SessionID)
	runCtx = context.WithValue(runCtx, tools.MessageIDContextKey, req.MessageID)
	call := fantasy.ToolCall{ID: req.ToolCallID, Name: AgentToolName, Input: string(req.Input)}
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

// batchCoordinatorDescendants is the R10a descendant reader. The coordinator
// driver runs its delegation to completion inside the turn, so after the
// turn ends there are no live descendants to observe (complete read).
type batchCoordinatorDescendants struct{}

func (batchCoordinatorDescendants) DescendantsLive(context.Context, batchAgentRunRequest) (bool, bool) {
	return false, true
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
func CoordinatorBatchAgentDeps() BatchAgentDeps {
	return batchAgentDeps{
		Runs:        batchCoordinatorAgentRuns{},
		Descendants: batchCoordinatorDescendants{},
	}
}
