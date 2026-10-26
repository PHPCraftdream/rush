package agent

import (
	"context"
	"errors"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

type batchGenericFamily struct{}

func newBatchGenericFamily() batchLeafFamily { return batchGenericFamily{} }

func (batchGenericFamily) start(ctx context.Context, env batchLeafEnv, l jobbatch.Launch, p batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if batchControlTarget(p.Tool) {
		return nil, &batchStartError{batchCodeNoLeaf, errors.New("control-plane target")}
	}
	leaf, err := env.resolveLeaf(p)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	// R11F round-2 (P1-5c): the PINNED policy wins. The run context is
	// rebuilt EXCLUSIVELY from the payload's immutable policy transport
	// (batchLeafRunContext: message id, image support, model name, session
	// id, folder scope, run allowlist, custom disk) — never from the Start
	// context's session-keyed lookups.
	if p.Policy == nil {
		// No pinned transport on the payload (older records, tests): keep
		// the Start context's leaf policy inputs, without retaining ctx.
		runCtx = context.WithValue(runCtx, tools.MessageIDContextKey, tools.GetMessageFromContext(ctx))
		runCtx = context.WithValue(runCtx, tools.SupportsImagesContextKey, tools.GetSupportsImagesFromContext(ctx))
		runCtx = context.WithValue(runCtx, tools.ModelNameContextKey, tools.GetModelNameFromContext(ctx))
	}
	runCtx = withBatchManagedCall(batchLeafRunContext(runCtx, p))
	// R12-H2 P1-4: batchLeafRunContext does NOT stamp the binding registry;
	// carry the App-owned registry so leaf-side binding resolution /
	// batchDiskRegistryFromContext resolve the SAME instance, never a
	// process global.
	runCtx = withDiskProviderRegistry(runCtx, env.DiskBindings)
	if p.diskProvider != nil && p.Policy != nil {
		runCtx = WithCallOptions(runCtx, boundCallOptions(p.diskProvider, p.Policy.DiskBindingID, env.DiskBindings))
	}
	e := &batchGenericExecution{cache: env.OutputCache, nodeID: l.ID, rootID: batchOutputRoot(l.ID), cancel: cancel, done: make(chan struct{})}
	call := fantasy.ToolCall{ID: p.ToolCallID, Name: p.Tool, Input: string(p.Input)}
	go e.run(runCtx, leaf, call, report)
	return e, nil
}

func batchControlTarget(name string) bool {
	switch name {
	case "await_tasks", "batch_run", "batch_add", "batch_status", "batch_output", "batch_stop",
		"ask_question", "wakein", "wakeon", "loop", "wake_list", "wake_cancel",
		"inspect_agent", "inject_agent", "stop_agent":
		return true
	}
	return false
}

type batchGenericExecution struct {
	cache          *batchOutputCache
	nodeID, rootID jobbatch.ID
	mu             sync.Mutex
	cancel         context.CancelFunc
	done           chan struct{}
	stopped        bool
	finished       bool
	disposed       bool
	content        []byte
}

var (
	_ jobbatch.Execution    = (*batchGenericExecution)(nil)
	_ jobbatch.OutputReader = (*batchGenericExecution)(nil)
	_ jobbatch.Disposer     = (*batchGenericExecution)(nil)
)

func (e *batchGenericExecution) run(ctx context.Context, leaf fantasy.AgentTool, call fantasy.ToolCall, report func(jobbatch.Result)) {
	resp, err, panicked := batchGenericRun(ctx, leaf, call)
	e.mu.Lock()
	cancelled := e.stopped && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
	result := batchResultFromResponse(resp, err, cancelled)
	if panicked {
		result = jobbatch.Result{State: jobbatch.StateFailed, Code: "tool_panic"}
	}
	e.content = []byte(resp.Content)
	if e.cache != nil {
		e.cache.putFor(e.nodeID, e.rootID, jobbatch.OutputChunk{Data: e.content, Next: uint64(len(e.content)), EOF: true})
	}
	e.finished = true
	close(e.done)
	e.mu.Unlock()
	e.cancel()
	report(result)
}

func batchGenericRun(ctx context.Context, leaf fantasy.AgentTool, call fantasy.ToolCall) (resp fantasy.ToolResponse, err error, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	resp, err = leaf.Run(ctx, call)
	return
}

func (e *batchGenericExecution) Stop(context.Context) error {
	e.mu.Lock()
	if !e.finished && !e.stopped {
		e.stopped = true
		e.cancel()
	}
	e.mu.Unlock()
	return nil
}

func (e *batchGenericExecution) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	select {
	case <-e.done:
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
	return jobbatch.OutputChunk{Data: append([]byte(nil), e.content[cursor:]...), Next: next, Oldest: 0, EOF: true}, nil
}

func (e *batchGenericExecution) Dispose(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.finished {
		return jobbatch.ErrNotTerminal
	}
	e.content = nil
	e.disposed = true
	return nil
}
