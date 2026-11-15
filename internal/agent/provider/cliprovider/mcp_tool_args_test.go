package cliprovider

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// flagValue returns the value following flag in args.
func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("%s missing from %v", flag, args)
	return ""
}

type stubBridge struct{ tools []BridgeTool }

func (b stubBridge) Tools() []BridgeTool { return b.tools }
func (stubBridge) Call(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{}, nil
}

type stubProxy struct{ tools []ExternalMCPTool }

func (p stubProxy) ListTools() []ExternalMCPTool { return p.tools }
func (stubProxy) CallTool(context.Context, string, string, string) (string, error) {
	return "", nil
}

// Under --dangerously-skip-permissions, --allowedTools blocks nothing: the
// claude CLI's native Bash/Write/Edit stayed usable and bypassed agentguard
// and the run policy. --tools must confine the built-in set.
func TestRushMCPToolArgsConfineCLIBuiltins(t *testing.T) {
	args := rushMCPToolArgs(nil, stubProxy{tools: []ExternalMCPTool{{ServerName: "gh", Name: "issues"}}})
	require.Equal(t, "WebSearch,WebFetch,Task,Agent", flagValue(t, args, "--tools"))
	allowed := flagValue(t, args, "--allowedTools")
	require.Contains(t, allowed, "mcp__rush__Write")
	require.Contains(t, allowed, "mcp__rush__gh__issues")
	require.Equal(t, "TodoWrite", flagValue(t, args, "--disallowedTools"))

	// A tool bridge exposes only its own tools: no CLI built-in at all.
	args = rushMCPToolArgs(stubBridge{tools: []BridgeTool{{WireName: "view"}}}, nil)
	require.Empty(t, flagValue(t, args, "--tools"))
	require.Equal(t, "mcp__rush__view", flagValue(t, args, "--allowedTools"))
}
