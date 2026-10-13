package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
)

// R11F (M3): CLI bridge wiring on the flag-on path. The bridge binds to the
// enclosing trusted invocation through the R7d pinnedInvoke (which closes
// over the ONE authority); its single generated call id and no-fallback
// behaviour live in cliprovider/tool_bridge.go and are untouched.

// coordinatorBridgeFallback is the cliprovider bridge seam: a stream whose
// invocation context carries no explicit bridge resolves through the LATEST
// pinned build registry of the process binding. No binding (flag-off), no
// latest pinned registry, or an unknown identity returns nil — the legacy
// "no bridge" behaviour, never a legacy dispatch substitute.
func coordinatorBridgeFallback(context.Context) cliprovider.ToolBridge {
	bind := processCoordinatorBatchBinding()
	if bind == nil {
		return nil
	}
	registryID := bind.latestRegistryID()
	if registryID == "" {
		return nil
	}
	registry, err := bind.ingress.Registry(registryID)
	if err != nil {
		return nil
	}
	bridge, err := newCLIToolBridge(registry, batchPinnedInvoke(bind.ingress, BatchInvocation{
		RegistryID: registryID,
		Origin:     "cli_bridge",
		Policy:     batchLeafPolicyContext{RegistryID: registryID, RestrictedToolset: registryID},
	}), nil)
	if err != nil {
		return nil
	}
	return bridge
}
