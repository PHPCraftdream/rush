package childagent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// BridgeInvocation is the trusted registry and depth metadata needed by the fallback.
type BridgeInvocation struct {
	RegistryID    string
	AgentDepth    int
	AgentDepthSet bool
}

// CoordinatorBridgeFallback resolves the registry and pin factory from one
// provider. A nil factory means no coordinator binding is available.
func CoordinatorBridgeFallback(
	ctx context.Context,
	invocationFrom func(context.Context) (BridgeInvocation, bool),
	provider func(BridgeInvocation) (*toolwrap.PolicyLeafRegistry, func(*session.RoleContract) PinnedInvoke, error),
) cliprovider.ToolBridge {
	if ctx == nil {
		return nil
	}
	inv, ok := invocationFrom(ctx)
	if !ok {
		return nil
	}
	registry, pin, err := provider(inv)
	if err != nil || pin == nil {
		return nil
	}
	roleContract := agentcall.CloneRoleContractForCall(toolwrap.RoleContractFromContext(ctx))
	if roleContract != nil {
		ctx = toolwrap.WithRoleToolContract(ctx, roleContract)
	}
	bridge, err := NewCLIBridge(ctx, registry, pin(roleContract), nil)
	if err != nil {
		return nil
	}
	return bridge
}
