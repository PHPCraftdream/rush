package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
)

// R11F (M3): CLI bridge wiring on the flag-on path. The bridge binds to the
// enclosing trusted invocation through the R7d pinnedInvoke (which closes
// over the ONE authority); its single generated call id and no-fallback
// behaviour live in cliprovider/tool_bridge.go and are untouched.

// coordinatorBridgeFallback checks the trusted invocation budget before resolving the pinned bridge.
func coordinatorBridgeFallback(ctx context.Context) cliprovider.ToolBridge {
	if ctx == nil {
		return nil
	}
	inv, ok := batchBridgeInvocationFrom(ctx)
	if !ok {
		return nil
	}
	bind := processCoordinatorBatchBinding()
	if bind == nil {
		return nil
	}
	registryID := inv.RegistryID
	registry, err := bind.ingress.Registry(registryID)
	if err != nil {
		return nil
	}
	bridgeCtx := ctx
	roleContract := cloneRoleContractForCall(roleContractFromContext(ctx))
	if roleContract != nil {
		bridgeCtx = withRoleToolContract(bridgeCtx, roleContract)
	}
	bridge, err := newCLIToolBridgeForContext(bridgeCtx, registry, batchPinnedInvoke(bind.ingress, BatchInvocation{
		RegistryID: inv.RegistryID,
		AgentDepth: inv.AgentDepth, AgentDepthSet: inv.AgentDepthSet,
		Origin: "cli_bridge",
		Policy: batchLeafPolicyContext{RegistryID: inv.RegistryID, RestrictedToolset: registryID, RoleContract: roleContract},
	}), nil)
	if err != nil {
		return nil
	}
	return bridge
}
