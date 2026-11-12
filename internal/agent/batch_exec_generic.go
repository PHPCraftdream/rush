package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
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
	if p.Policy != nil && p.Policy.RoleContract != nil {
		if err := p.Policy.RoleContract.Validate(); err != nil {
			return nil, &batchStartError{batchCodeBadPayload, err}
		}
		if !roleContractAllowsTool(p.Policy.RoleContract, p.Tool) {
			return nil, &batchStartError{batchCodeNoLeaf, fmt.Errorf("role tool denied: %s", p.Tool)}
		}
	}
	if p.SelectedRoleContract != nil {
		if err := validateSelectedBatchRole(p.SelectedRoleContract, p.SelectedRoleContract.Name, func() *session.RoleContract {
			if p.Policy == nil {
				return nil
			}
			return p.Policy.RoleContract
		}()); err != nil {
			return nil, &batchStartError{batchCodeBadPayload, err}
		}
	}
	leaf, err := env.resolveLeaf(p)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	if p.Policy == nil {
		runCtx = context.WithValue(runCtx, tools.MessageIDContextKey, tools.GetMessageFromContext(ctx))
		runCtx = context.WithValue(runCtx, tools.SupportsImagesContextKey, tools.GetSupportsImagesFromContext(ctx))
		runCtx = context.WithValue(runCtx, tools.ModelNameContextKey, tools.GetModelNameFromContext(ctx))
	}
	if p.Policy != nil && p.Policy.RoleContract != nil {
		runCtx = withRoleToolContract(runCtx, p.Policy.RoleContract)
	}
	if p.SelectedRoleContract != nil {
		runCtx = WithSelectedRoleContract(runCtx, p.SelectedRoleContract)
	}
	runCtx = withBatchManagedCall(batchLeafRunContext(runCtx, p))
	// Restore trusted inherited and selected caps after any call-options
	// rebuild performed while decorating the leaf execution context.
	if p.Policy != nil && p.Policy.RoleContract != nil {
		runCtx = WithCallRoleContract(runCtx, p.Policy.RoleContract)
	}
	if p.SelectedRoleContract != nil {
		runCtx = WithSelectedRoleContract(runCtx, p.SelectedRoleContract)
	}
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
