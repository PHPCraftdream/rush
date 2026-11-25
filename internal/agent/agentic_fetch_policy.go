package agent

import (
	"context"
	"errors"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/installwire"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/google/uuid"
)

type privateFetchIngressKey struct{}

// privateFetchIngressTool is the root compatibility alias over the moved
// private fetch ingress shell
// (internal/agent/runtime/policy/toolwrap/ingress.go); the ingress
// assertions keep compiling until their files move.
type privateFetchIngressTool = toolwrap.PrivateFetchIngressTool

// newPrivateFetchIngressShell arms one private tool through the moved
// shell: the root-owned pinned registry and the pre-built childagent.PinnedInvoke
// pass through the shell's Registry/Invoke fields — no package-agent
// identifiers cross into toolwrap.
func newPrivateFetchIngressShell(tool fantasy.AgentTool, leaves *toolwrap.PolicyLeafRegistry, invoke childagent.PinnedInvoke) *toolwrap.PrivateFetchIngressTool {
	return &toolwrap.PrivateFetchIngressTool{
		AgentTool: tool,
		Registry:  leaves,
		Invoke: func(ctx context.Context, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return invoke(ctx, leaf, call)
		},
	}
}

func withPrivateFetchIngress(ctx context.Context, invoke childagent.PinnedInvoke) context.Context {
	return context.WithValue(ctx, privateFetchIngressKey{}, invoke)
}

// buildPrivateFetchTools builds the delegated sub-agent's private toolset.
// R12-H2a round 2: the CALLER owns the build-resource scope and passes it
// explicitly — this function reads nothing from ctx for scoping and has NO
// fallback. A nil scope with an armed ingress fails closed through
// armPrivateFetchIngress.
func buildPrivateFetchTools(ctx context.Context, list []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer, scope *buildResourceScope) toolwrap.PolicyToolBuild {
	built := toolwrap.NewPolicyToolBuildForContext(ctx, toolwrap.DecoratePolicyTools(list, authorizer, nil, true))
	// R11F (M3) flag-on: resolve the ONE authority (explicit ctx binding
	// only — the process-global fallback is eliminated, R12-H2 §1.3), pin
	// this build's leaves under a fresh identity and re-arm every private
	// tool through the R7d childagent.PinnedInvoke. No binding leaves the legacy
	// in-process tools.
	ingress := privateFetchIngressFor(ctx)
	if ingress != nil {
		return armPrivateFetchIngress(ctx, built, ingress, scope)
	}
	invoke, _ := ctx.Value(privateFetchIngressKey{}).(childagent.PinnedInvoke)
	if invoke == nil {
		return built
	}
	ingressList := make([]fantasy.AgentTool, len(built.Tools))
	for i, tool := range built.Tools {
		ingressList[i] = newPrivateFetchIngressShell(tool, built.Leaves, invoke)
	}
	built.Tools = ingressList
	return built
}

// privateFetchIngressFor resolves the batch ingress for the private fetch
// build: ONLY an explicitly bound value. R12-H2 §1.3: the provider-bound
// process-global fallback is eliminated — delegation/private-fetch
// resolution never touches processCoordinatorBatchBinding.
func privateFetchIngressFor(ctx context.Context) BatchWorkIngress {
	ingress, _ := ctx.Value(privateFetchIngressKey{}).(BatchWorkIngress)
	return ingress
}

// armPrivateFetchIngress pins the build's leaf registry and wraps every
// private tool so its Run enters the ONE authority. A pin failure fails
// closed: the tools refuse rather than fall back to the old path.
// R11F round-2 (P1-5c): the invocation Policy template carries the pinned
// RegistryID/RestrictedToolset identity; see batchArmingPolicyNote (batch_
// install_policy.go) for the documented contract on FolderScope/RunAllowlist/
// CustomDisk at this arming point.
// R12-H2 §1.2: the pin takes a Construction owner released in the call's
// build-resource scope; a missing manager or scope fails closed.
func armPrivateFetchIngress(ctx context.Context, built toolwrap.PolicyToolBuild, ingress BatchWorkIngress, scope *buildResourceScope) toolwrap.PolicyToolBuild {
	registryID, err := ingress.PinLeafRegistry(built.Leaves)
	if err == nil {
		// Record the pinned identity on the build so owners of the build can
		// observe the CONSTRUCTION they pin (the capability manager deletes
		// the identity index entry when its last owner goes).
		built.RegistryID = registryID
		var caps *diskCapabilityManager
		if bw, ok := ingress.(*batchWorkIngress); ok {
			caps = bw.caps
		}
		switch {
		case caps == nil:
			err = ErrDiskProviderUnavailable
		case scope == nil:
			err = errors.New("agentic fetch: no build-resource scope in the trusted call state")
		default:
			constructionKey := diskOwnerKey{kind: diskOwnerConstruction, instance: uuid.NewString(), registryID: registryID}
			if err = caps.acquireConstruction("", registryID, constructionKey.instance); err == nil {
				scope.Register(func() { caps.release(constructionKey) })
			}
		}
	}
	if err != nil {
		for i, tool := range built.Tools {
			built.Tools[i] = errBatchIngressTool{AgentTool: tool, err: err}
		}
		return built
	}
	tpl := BatchInvocation{
		RegistryID: registryID,
		Origin:     "agentic_fetch",
		// R12-H2a (P1-3): the restricted toolset is the REAL toolset
		// fingerprint of the pinned build (the executor's hasToolset checks
		// the comma-joined leaf names), not the registry ID — a
		// provider-backed private payload must pass hasToolset at the
		// executor without bypassing it.
		Policy: batchLeafPolicyContext{RegistryID: registryID, RestrictedToolset: installwire.ToolsetFingerprint(built.Tools), RoleContract: agentcall.CloneRoleContractForCall(toolwrap.RoleContractForToolBuild(ctx, built.Tools))},
	}
	invoke := batchPinnedInvoke(ingress, tpl)
	ingressList := make([]fantasy.AgentTool, len(built.Tools))
	for i, tool := range built.Tools {
		ingressList[i] = newPrivateFetchIngressShell(tool, built.Leaves, invoke)
	}
	built.Tools = ingressList
	return built
}
