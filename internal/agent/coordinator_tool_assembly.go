package agent

import (
	"context"

	"charm.land/fantasy"
	toolwrap "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/tools/hooks"
	"github.com/PHPCraftdream/rush/internal/config"
)

// finishToolAssembly is the final construction boundary. Flag OFF (no batch
// binding): the historical branch, byte-for-byte — async wrappers, then
// hooks/restrictions, then the stall-detach wrapper. Flag ON: hooks and
// restrictions run once here, then every batch-admissible work tool is
// re-armed through the ONE authority (batchRuntime.Invoke) and the old
// async/stall wrappers are NOT installed. allowed is the build's control
// grant list: batchControlAvailability over the final filtered tools and
// AllowedTools, computed once in buildPolicyTools.
func (c *coordinator) finishToolAssembly(ctx context.Context, filteredTools []fantasy.AgentTool, hookRunner *hooks.Runner, isSubAgent bool, allowed []string, configs ...*config.Config) (toolwrap.PolicyToolBuild, error) {
	return newToolsComponent(c).finishToolAssembly(ctx, filteredTools, hookRunner, isSubAgent, allowed, configs...)
}
