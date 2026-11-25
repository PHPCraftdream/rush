package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// batchArgvFamily is inert until explicitly registered with a batchExecutor.
// It uses the pinned policy leaf, never a raw run_command factory.
type batchArgvFamily struct{}

var _ batchLeafFamily = batchArgvFamily{}

func (batchArgvFamily) start(ctx context.Context, env batchLeafEnv, l jobbatch.Launch, p batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Tool != tools.RunCommandToolName {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, errors.New("argv requires run_command"))
	}
	if p.Policy != nil && p.Policy.RoleContract != nil && !roleContractAllowsTool(p.Policy.RoleContract, p.Tool) {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, fmt.Errorf("role tool denied: %s", p.Tool))
	}
	leaf, err := env.resolveLeaf(p)
	if err != nil {
		return nil, err
	}
	if leaf == nil || leaf.Info().Name != tools.RunCommandToolName {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, errors.New("pinned argv leaf is not run_command"))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Acceptance is the boundary: subsequent caller cancellation cannot reject
	// setup and abandon a process. Values survive, lifetime belongs to Stop.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// R11F round-2 (P1-5c): the PINNED policy wins — rebuilt exclusively
	// from the payload's immutable policy transport.
	runCtx = batchLeafRunContext(runCtx, p)
	if p.Policy != nil && p.Policy.RoleContract != nil {
		runCtx = toolwrap.WithRoleToolContract(runCtx, p.Policy.RoleContract)
	}
	if p.SelectedRoleContract != nil {
		runCtx = WithSelectedRoleContract(runCtx, p.SelectedRoleContract)
	}
	e := newRetainedArgv(cancel, env.OutputCache, l.ID, batchmeta.OutputRoot(l.ID))
	runCtx = e.WithArgvHandle(runCtx)
	go e.Run(runCtx, leaf, fantasy.ToolCall{ID: p.ToolCallID, Name: p.Tool, Input: string(p.Input)}, report)
	return e, nil
}

func newRetainedArgv(cancel context.CancelFunc, cache *batchOutputCache, id, root jobbatch.ID) *batchexec.ArgvExecution {
	output := batchExecutionOutput(cache, id, root, batchmeta.OutputCursorArgvRestart)
	var capture func(batchexec.ArgvHandle)
	if cache != nil {
		capture = func(p batchexec.ArgvHandle) {
			_ = batchOutputCapture(cache, id, root, batchArgvOutputReader{p}, batchmeta.OutputCursorArgvRestart)
		}
	}
	return batchexec.NewArgv(cancel, output, capture)
}
