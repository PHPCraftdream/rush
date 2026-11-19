package agent

import (
	"context"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools/hooks"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// ErrRoleToolDenied keeps the root identity for frozen ingress call sites.
var ErrRoleToolDenied = toolwrap.ErrRoleToolDenied

// policyToolBuild is a frozen-file shim over the moved manifest build type.
type policyToolBuild = toolwrap.PolicyToolBuild

// policyLeafRegistry is a frozen-file shim over the moved registry type.
type policyLeafRegistry = toolwrap.PolicyLeafRegistry

// Frozen-file shims below (agentic_fetch_policy.go, batch_install_tools.go,
// batch_child_tool_scope.go, agentic_fetch_tool.go cannot be edited by this
// chunk); every editable caller qualifies toolwrap directly.

func newPolicyToolBuildForContext(ctx context.Context, tools []fantasy.AgentTool) policyToolBuild {
	return toolwrap.NewPolicyToolBuildForContext(ctx, tools)
}

func decoratePolicyTools(tools []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer, runner *hooks.Runner, child bool) []fantasy.AgentTool {
	return toolwrap.DecoratePolicyTools(tools, authorizer, runner, child)
}

func roleContractForToolBuild(ctx context.Context, tools []fantasy.AgentTool) *session.RoleContract {
	return toolwrap.RoleContractForToolBuild(ctx, tools)
}

func roleContractsForTools(tools []fantasy.AgentTool) []*session.RoleContract {
	return toolwrap.RoleContractsForTools(tools)
}

func roleContractsAllowName(contracts []*session.RoleContract, name string) bool {
	return toolwrap.RoleContractsAllowName(contracts, name)
}

func roleContractAllowsName(contract *session.RoleContract, name string) bool {
	return toolwrap.RoleContractAllowsName(contract, name)
}

func withRoleToolContract(ctx context.Context, contract *session.RoleContract) context.Context {
	return toolwrap.WithRoleToolContract(ctx, contract)
}

func guardRoleToolCall(ctx context.Context, name string) error {
	return toolwrap.GuardRoleToolCall(ctx, name)
}

func roleToolContractsFrom(ctx context.Context) []*session.RoleContract {
	return toolwrap.RoleToolContractsFrom(ctx)
}

func roleToolAllowed(ctx context.Context, name string) bool {
	return toolwrap.RoleToolAllowed(ctx, name)
}

func roleToolsConstrained(ctx context.Context) bool {
	return toolwrap.RoleToolsConstrained(ctx)
}

func roleContractFromContext(ctx context.Context) *session.RoleContract {
	return toolwrap.RoleContractFromContext(ctx)
}

func newPolicyToolBuild(tools []fantasy.AgentTool) policyToolBuild {
	return toolwrap.NewPolicyToolBuild(tools)
}

// RoleContractAllowsTool keeps the root identity for the frozen call sites.
func RoleContractAllowsTool(contract *session.RoleContract, name string) bool {
	return toolwrap.RoleContractAllowsTool(contract, name)
}

func filterRoleContractTools(ctx context.Context, tools []fantasy.AgentTool) []fantasy.AgentTool {
	return toolwrap.FilterRoleContractTools(ctx, tools)
}
