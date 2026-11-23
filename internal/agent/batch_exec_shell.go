package agent

import (
	"context"
	"errors"
	"fmt"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

type batchShellFamily struct{}

var _ batchLeafFamily = batchShellFamily{}

func (batchShellFamily) start(ctx context.Context, env batchLeafEnv, l jobbatch.Launch, p batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Tool != tools.BashToolName {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, errors.New("shell kind requires bash"))
	}
	if p.Policy != nil && p.Policy.RoleContract != nil && !roleContractAllowsTool(p.Policy.RoleContract, p.Tool) {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, fmt.Errorf("role tool denied: %s", p.Tool))
	}
	leaf, err := env.resolveLeaf(p)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// R11F round-2 (P1-5c): the PINNED policy wins — rebuilt exclusively
	// from the payload's immutable policy transport.
	runCtx = batchLeafRunContext(runCtx, p)
	if p.Policy != nil && p.Policy.RoleContract != nil {
		runCtx = withRoleToolContract(runCtx, p.Policy.RoleContract)
	}
	if p.SelectedRoleContract != nil {
		runCtx = WithSelectedRoleContract(runCtx, p.SelectedRoleContract)
	}
	e := newRetainedShell(cancel, env.OutputCache, l.ID, batchmeta.OutputRoot(l.ID))
	runCtx = e.WithShellHandle(runCtx)
	go e.Run(runCtx, leaf, fantasy.ToolCall{ID: p.ToolCallID, Name: p.Tool, Input: string(p.Input)}, report)
	return e, nil
}

func newRetainedShell(cancel context.CancelFunc, cache *batchOutputCache, id, root jobbatch.ID) *batchexec.ShellExecution {
	output := batchExecutionOutput(cache, id, root, batchmeta.OutputCursorClamp)
	var capture func(jobbatch.OutputReader)
	if cache != nil {
		capture = func(reader jobbatch.OutputReader) {
			_ = batchOutputCapture(cache, id, root, reader, batchmeta.OutputCursorClamp)
		}
	}
	return batchexec.NewShell(cancel, output, capture)
}
