package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

type batchGenericFamily struct{}

func newBatchGenericFamily() batchLeafFamily { return batchGenericFamily{} }

func (batchGenericFamily) start(ctx context.Context, env batchLeafEnv, l jobbatch.Launch, p batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if batchControlTarget(p.Tool) {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, errors.New("control-plane target"))
	}
	if p.Policy != nil && p.Policy.RoleContract != nil {
		if err := p.Policy.RoleContract.Validate(); err != nil {
			return nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, err)
		}
		if !roleContractAllowsTool(p.Policy.RoleContract, p.Tool) {
			return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, fmt.Errorf("role tool denied: %s", p.Tool))
		}
	}
	if p.SelectedRoleContract != nil {
		if err := batchinst.ValidateSelectedRole(p.SelectedRoleContract, p.SelectedRoleContract.Name, func() *session.RoleContract {
			if p.Policy == nil {
				return nil
			}
			return p.Policy.RoleContract
		}()); err != nil {
			return nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, err)
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
		runCtx = toolwrap.WithRoleToolContract(runCtx, p.Policy.RoleContract)
	}
	if p.SelectedRoleContract != nil {
		runCtx = WithSelectedRoleContract(runCtx, p.SelectedRoleContract)
	}
	runCtx = batchlife.WithBatchManagedCall(batchLeafRunContext(runCtx, p))
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
	e := batchexec.NewGeneric(cancel, batchExecutionOutput(env.OutputCache, l.ID, batchmeta.OutputRoot(l.ID), batchmeta.OutputCursorClamp))
	call := fantasy.ToolCall{ID: p.ToolCallID, Name: p.Tool, Input: string(p.Input)}
	go e.Run(runCtx, leaf, call, report)
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

func batchExecutionOutput(cache *batchOutputCache, id, root jobbatch.ID, contract batchmeta.OutputCursorContract) batchexec.Output {
	output := batchexec.Output{Result: batchdispatch.ResultFromResponse}
	if cache != nil {
		output.Store = func(chunk jobbatch.OutputChunk) { cache.putForContract(id, root, chunk, contract) }
	}
	return output
}
