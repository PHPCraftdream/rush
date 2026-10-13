package agent

import (
	"context"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
)

type privateFetchIngressKey struct{}

func withPrivateFetchIngress(ctx context.Context, invoke pinnedInvoke) context.Context {
	return context.WithValue(ctx, privateFetchIngressKey{}, invoke)
}

func buildPrivateFetchTools(ctx context.Context, list []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer) policyToolBuild {
	built := newPolicyToolBuild(decoratePolicyTools(list, authorizer, nil, true))
	// R11F (M3) flag-on: resolve the ONE authority (explicit ctx binding
	// first, then the process binding), pin this build's leaves under a
	// fresh identity and re-arm every private tool through the R7d
	// pinnedInvoke. No binding leaves the legacy in-process tools.
	ingress := privateFetchIngressFor(ctx)
	if ingress != nil {
		return armPrivateFetchIngress(built, ingress)
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
// build: an explicitly bound value first, else the process binding.
func privateFetchIngressFor(ctx context.Context) BatchWorkIngress {
	if ingress, ok := ctx.Value(privateFetchIngressKey{}).(BatchWorkIngress); ok && ingress != nil {
		return ingress
	}
	if bind := processCoordinatorBatchBinding(); bind != nil {
		return bind.ingress
	}
	return nil
}

// armPrivateFetchIngress pins the build's leaf registry and wraps every
// private tool so its Run enters the ONE authority. A pin failure fails
// closed: the tools refuse rather than fall back to the old path.
func armPrivateFetchIngress(built policyToolBuild, ingress BatchWorkIngress) policyToolBuild {
	registryID, err := ingress.PinLeafRegistry(built.leaves)
	if err != nil {
		for i, tool := range built.tools {
			built.tools[i] = errBatchIngressTool{AgentTool: tool, err: err}
		}
		return built
	}
	tpl := BatchInvocation{
		RegistryID: registryID,
		Origin:     "agentic_fetch",
		Policy:     batchLeafPolicyContext{RegistryID: registryID, RestrictedToolset: registryID},
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
