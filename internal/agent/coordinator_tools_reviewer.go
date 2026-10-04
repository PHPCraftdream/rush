// Reviewer-role tool gating: the read-only filter for a call whose
// CallOptions.ModelRole is the reviewer slot. Extracted next to
// coordinator_tools.go's other per-call filters (DisableSubAgents, folder
// scope) so the ordering between them is visible at the buildTools call site.

package agent

import (
	"context"
	"slices"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
)

// reviewerReadOnlyToolNames: the only tools a reviewer-role top-level call may hold.
var reviewerReadOnlyToolNames = []string{
	tools.ViewToolName, tools.GrepToolName, tools.GlobToolName, tools.LSToolName,
	tools.GitReadToolName, tools.ReadDelegationTranscriptToolName,
	tools.FSReadToolName, tools.FSListToolName, tools.FSFindToolName, tools.FSGrepToolName,
}

// applyCallReviewerReadOnly narrows a reviewer-role top-level call to the
// read-only tool names in reviewerReadOnlyToolNames. The gate is the per-call
// CallOptions role, exactly like applyCallDisableSubAgents: a context without
// CallOptions, a non-reviewer role, or a sub-agent changes nothing.
//
// It MUST run after applyCallFolderScope: a scoped call's grants re-add fs_*
// tools to the list, and the read-only filter has to see that final list to
// drop fs_write again. MCP is dropped wholesale -- an external filesystem MCP
// server would read whatever the scope denied.
func applyCallReviewerReadOnly(ctx context.Context, agent config.Agent, isSubAgent bool) config.Agent {
	opts := callOptionsFrom(ctx)
	if opts == nil || opts.ModelRole != config.SelectedModelTypeReviewer || isSubAgent {
		return agent
	}
	allowed := make([]string, 0, len(reviewerReadOnlyToolNames))
	for _, name := range agent.AllowedTools {
		if slices.Contains(reviewerReadOnlyToolNames, name) {
			allowed = append(allowed, name)
		}
	}
	agent.AllowedTools = allowed
	agent.AllowedMCP = map[string][]string{}
	return agent
}
