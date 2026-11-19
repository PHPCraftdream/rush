package agent

// Temporary DM4 chunk 2 bridge (internal-only). The stable tool wrappers now
// live in internal/agent/runtime/policy/toolwrap with their original
// lowercase concrete types; these thin delegates keep the staying PRODUCTION
// root files (tools_component.go, agent_turn*.go, step_progress.go) working
// until they move in later chunks.
//
// CLEANUP PLAN (must be completed before final): delete this file wholesale
// once its callers have moved or been requalified to toolwrap directly. The
// only production exports on the toolwrap side are the narrow ones used here
// (FatalToolCollector, RestrictedToolAction, RestrictedRunAuthorizer,
// WithFatalToolErrors, WithProviderOptionsOnLast, WrapToolsWith*); the
// wrapper concrete types and the restricted marker stay package-private in
// toolwrap. Tests must NOT depend on this file — they import toolwrap
// directly.

import (
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools/hooks"
)

// fatalToolCollector stays nameable in the root PRODUCTION wiring: the turn
// files hold and drain it directly (agent_turn.go / agent_turn_stream.go).
type fatalToolCollector = toolwrap.FatalToolCollector

func restrictedRunAuthorizer(service permission.Service) permission.RestrictedRunAuthorizer {
	return toolwrap.RestrictedRunAuthorizer(service)
}

func wrapToolsWithHooks(tools []fantasy.AgentTool, runner *hooks.Runner, isSubAgent bool) []fantasy.AgentTool {
	return toolwrap.WrapToolsWithHooks(tools, runner, isSubAgent)
}

func wrapToolsWithRestrictedRun(tools []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer) []fantasy.AgentTool {
	return toolwrap.WrapToolsWithRestrictedRun(tools, authorizer)
}

func wrapToolsWithErrorLogging(list []fantasy.AgentTool) []fantasy.AgentTool {
	return toolwrap.WrapToolsWithErrorLogging(list)
}

func withFatalToolErrors(tools []fantasy.AgentTool, c *fatalToolCollector) []fantasy.AgentTool {
	return toolwrap.WithFatalToolErrors(tools, c)
}

func withProviderOptionsOnLast(tools []fantasy.AgentTool, opts fantasy.ProviderOptions) []fantasy.AgentTool {
	return toolwrap.WithProviderOptionsOnLast(tools, opts)
}
