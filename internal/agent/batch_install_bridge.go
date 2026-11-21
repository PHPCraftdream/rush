package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// coordinatorBridgeFallback wires the trusted invocation and the one ingress
// authority into the CLI bridge. The bridge's call ID and no-fallback behavior
// remain in cliprovider.
func coordinatorBridgeFallback(ctx context.Context) cliprovider.ToolBridge {
	return childagent.CoordinatorBridgeFallback(ctx,
		func(ctx context.Context) (childagent.BridgeInvocation, bool) {
			inv, ok := batchBridgeInvocationFrom(ctx)
			return childagent.BridgeInvocation{
				RegistryID: inv.RegistryID, AgentDepth: inv.AgentDepth, AgentDepthSet: inv.AgentDepthSet,
			}, ok
		},
		func(inv childagent.BridgeInvocation) (*BatchLeafRegistry, func(*session.RoleContract) childagent.PinnedInvoke, error) {
			bind := processCoordinatorBatchBinding()
			if bind == nil {
				return nil, nil, nil
			}
			ingress := bind.ingress
			registry, err := ingress.Registry(inv.RegistryID)
			if err != nil {
				return nil, nil, err
			}
			return registry, func(roleContract *session.RoleContract) childagent.PinnedInvoke {
				return batchPinnedInvoke(ingress, BatchInvocation{
					RegistryID: inv.RegistryID,
					AgentDepth: inv.AgentDepth, AgentDepthSet: inv.AgentDepthSet,
					Origin: "cli_bridge",
					Policy: batchLeafPolicyContext{RegistryID: inv.RegistryID, RestrictedToolset: inv.RegistryID, RoleContract: roleContract},
				})
			}, nil
		},
	)
}
