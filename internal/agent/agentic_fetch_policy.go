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
	invoke, _ := ctx.Value(privateFetchIngressKey{}).(pinnedInvoke)
	if invoke == nil {
		return built
	}
	ingress := make([]fantasy.AgentTool, len(built.tools))
	for i, tool := range built.tools {
		ingress[i] = &privateFetchIngressTool{AgentTool: tool, registry: built.leaves, invoke: invoke}
	}
	built.tools = ingress
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
