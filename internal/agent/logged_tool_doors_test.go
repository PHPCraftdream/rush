package agent

// The wrapper moved to internal/agent/runtime/policy/toolwrap (DM4 chunk 2);
// these two door tests stay in the root because they pin the CONSTRUCTOR
// doors (NewSessionAgent / SetTools), not the wrapper itself. The concrete
// logged wrapper type is now unexported in toolwrap, so identity is checked
// through the wrapper's own idempotency port instead of a type assertion:
// wrapToolsWithErrorLogging returns the input unchanged exactly when it is
// already the logged wrapper, so Same proves the door installed it — and a
// bare stub would be re-wrapped, failing the Same check.

import (
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/stretchr/testify/require"
)

// Wrapping happens at the doors into a sessionAgent, not at the places a
// tool slice is assembled. That is the whole point: the wrapper was first
// applied at the buildTools call site, and agentic_fetch — which builds its
// own six tools and never goes through buildTools — was silently left out.
// These two tests fail if either door stops wrapping, which is what makes
// "every tool, every path" checkable instead of a promise in a comment.
func TestSessionAgent_ConstructorWrapsToolsForLogging(t *testing.T) {
	agent := NewSessionAgent(SessionAgentOptions{
		Tools: []fantasy.AgentTool{stubTool{name: "view"}, stubTool{name: "bash"}},
	}).(*sessionAgent)

	for _, tool := range agent.tools.Copy() {
		require.Same(t, tool, toolwrap.WrapToolsWithErrorLogging([]fantasy.AgentTool{tool})[0],
			"a tool handed to NewSessionAgent must be wrapped; agentic_fetch reaches "+
				"the agent this way and no other")
	}
}

func TestSessionAgent_SetToolsWrapsToolsForLogging(t *testing.T) {
	agent := NewSessionAgent(SessionAgentOptions{}).(*sessionAgent)
	agent.SetTools([]fantasy.AgentTool{stubTool{name: "view"}})

	tools := agent.tools.Copy()
	require.Len(t, tools, 1)
	require.Same(t, tools[0], toolwrap.WrapToolsWithErrorLogging([]fantasy.AgentTool{tools[0]})[0],
		"the coordinator installs its tools through SetTools, not the constructor")
}
