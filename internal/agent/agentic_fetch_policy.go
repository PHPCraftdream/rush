package agent

import (
	"context"
	"errors"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/google/uuid"
)

type privateFetchIngressKey struct{}

func withPrivateFetchIngress(ctx context.Context, invoke pinnedInvoke) context.Context {
	return context.WithValue(ctx, privateFetchIngressKey{}, invoke)
}

// buildPrivateFetchTools builds the delegated sub-agent's private toolset.
// R12-H2a round 2: the CALLER owns the build-resource scope and passes it
// explicitly — this function reads nothing from ctx for scoping and has NO
// fallback. A nil scope with an armed ingress fails closed through
// armPrivateFetchIngress.
func buildPrivateFetchTools(ctx context.Context, list []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer, scope *buildResourceScope) policyToolBuild {
	built := newPolicyToolBuild(decoratePolicyTools(list, authorizer, nil, true))
	// R11F (M3) flag-on: resolve the ONE authority (explicit ctx binding
	// only — the process-global fallback is eliminated, R12-H2 §1.3), pin
	// this build's leaves under a fresh identity and re-arm every private
	// tool through the R7d pinnedInvoke. No binding leaves the legacy
	// in-process tools.
	ingress := privateFetchIngressFor(ctx)
	if ingress != nil {
		return armPrivateFetchIngress(built, ingress, scope)
	}
	invoke, _ := ctx.Value(privateFetchIngressKey{}).(pinnedInvoke)
	if invoke == nil {
		return built
	}
	ingressList := make([]fantasy.AgentTool, len(built.tools))
	for i, tool := range built.tools {
		ingressList[i] = &privateFetchIngressTool{AgentTool: tool, registry: built.leaves, invoke: invoke}
	}
	built.tools = ingressList
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
func armPrivateFetchIngress(built policyToolBuild, ingress BatchWorkIngress, scope *buildResourceScope) policyToolBuild {
	registryID, err := ingress.PinLeafRegistry(built.leaves)
	if err == nil {
		// Record the pinned identity on the build so owners of the build can
		// observe the CONSTRUCTION they pin (the capability manager deletes
		// the identity index entry when its last owner goes).
		built.registryID = registryID
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
				scope.register(func() { caps.release(constructionKey) })
			}
		}
	}
	if err != nil {
		for i, tool := range built.tools {
			built.tools[i] = errBatchIngressTool{AgentTool: tool, err: err}
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
		Policy: batchLeafPolicyContext{RegistryID: registryID, RestrictedToolset: batchToolsetFingerprint(built.tools)},
	}
	invoke := batchPinnedInvoke(ingress, tpl)
	ingressList := make([]fantasy.AgentTool, len(built.tools))
	for i, tool := range built.tools {
		ingressList[i] = &privateFetchIngressTool{AgentTool: tool, registry: built.leaves, invoke: invoke}
	}
	built.tools = ingressList
	return built
}

type privateFetchIngressTool struct {
	fantasy.AgentTool
	registry *policyLeafRegistry
	invoke   pinnedInvoke
}

func (t *privateFetchIngressTool) restrictedRunWrapped() {}

func (t *privateFetchIngressTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	leaf, err := t.registry.resolve(t.Info().Name)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	call.Name = t.Info().Name
	return t.invoke(ctx, leaf, call)
}
