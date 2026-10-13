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
// invocation context carries an explicit TRUSTED bridge-invocation stamp
// (P1-4: stamped by the agent-family driver on the delegation leaf's run
// context — withBatchBridgeInvocation) resolves through THAT invocation's
// pinned registry. R11F P1-4: the old "latest pinned registry of the
// process" lookup is gone — it let two concurrent invocations with different
// pinned registries cross-contaminate. No stamp (the coordinator's own
// non-batch path), no process binding, an unknown identity, or a stamp
// without a registry id returns nil — the legacy "no bridge" behaviour,
// never a legacy dispatch substitute.
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
