package cliprovider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyRestrictedToolArgsConstrainedClaude(t *testing.T) {
	builtins, err := BuiltinBridgeTools()
	require.NoError(t, err)
	bridge := testToolBridge{tools: builtins}
	spec := CLISpec{ModelID: "claude-test", Binary: "claude", SupportsRestrictedTools: true}
	policy := ToolPolicy{Allowed: []string{"view"}, Constrained: true}
	args, err := applyRestrictedToolArgs([]string{
		"--model", "claude-test",
		"--tools", "Bash,Write,WebSearch",
		"--allowedTools", "Bash,WebSearch",
		"--allowed-tools", "WebFetch",
		"--disallowedTools", "Read",
		"--dangerously-skip-permissions",
	}, spec, bridge, nil, policy)
	require.NoError(t, err)
	require.Equal(t, []string{
		"--model", "claude-test",
		"--dangerously-skip-permissions",
		"--tools", "",
		"--allowedTools", "mcp__rush__Read",
		"--disallowedTools", "TodoWrite",
	}, args)
}

func TestRestrictedToolArgsExplicitEmptyPolicyDeniesAll(t *testing.T) {
	builtins, err := BuiltinBridgeTools()
	require.NoError(t, err)
	bridge := testToolBridge{tools: builtins}
	policy := ToolPolicy{Constrained: true}
	spec := CLISpec{ModelID: "claude-test", Binary: "claude", SupportsRestrictedTools: true}

	args, err := applyRestrictedToolArgs(nil, spec, bridge, nil, policy)
	require.NoError(t, err)
	require.Equal(t, []string{"--tools", "", "--allowedTools", "", "--disallowedTools", "TodoWrite"}, args)
	require.Equal(t, []string{"--tools", "", "--allowedTools", "", "--disallowedTools", "TodoWrite"}, rushMCPToolArgsWithPolicy(bridge, nil, policy))
}

func TestRestrictedToolArgsPinnedBridgeUsesCanonicalPolicyNames(t *testing.T) {
	builtins, err := BuiltinBridgeTools()
	require.NoError(t, err)
	bridgeTools := append([]BridgeTool(nil), builtins...)
	bridgeTools = append(bridgeTools, BridgeTool{WireName: "ReadAlias", CanonicalName: "bash"})
	bridge := testToolBridge{tools: bridgeTools}
	policy := ToolPolicy{Allowed: []string{"view"}, Constrained: true}

	require.Equal(t, []string{"--tools", "", "--allowedTools", "mcp__rush__Read", "--disallowedTools", "TodoWrite"}, rushMCPToolArgsWithPolicy(bridge, nil, policy))
	args, err := applyRestrictedToolArgs(nil, CLISpec{ModelID: "claude-test", Binary: "claude", SupportsRestrictedTools: true}, bridge, nil, policy)
	require.NoError(t, err)
	require.Equal(t, []string{"--tools", "", "--allowedTools", "mcp__rush__Read", "--disallowedTools", "TodoWrite"}, args)
}

func TestRushMCPToolArgsUnconstrainedRetainsLegacyAllowlist(t *testing.T) {
	require.Equal(t, []string{
		"--tools", "WebSearch,WebFetch,Task,Agent",
		"--allowedTools", "mcp__rush__Bash,mcp__rush__Read,mcp__rush__Write,mcp__rush__Glob,mcp__rush__Grep,mcp__rush__todos,WebSearch,WebFetch,Task,Agent",
		"--disallowedTools", "TodoWrite",
	}, rushMCPToolArgsWithPolicy(nil, nil, ToolPolicy{}))
}

func TestApplyRestrictedToolArgsUnsupportedSpecFailsClosed(t *testing.T) {
	_, err := applyRestrictedToolArgs(nil, CLISpec{ModelID: "unsupported", Binary: "claude"}, nil, nil, ToolPolicy{Constrained: true})
	require.ErrorContains(t, err, "cannot enforce this invocation's native-tool capability ceiling")

	_, err = applyRestrictedToolArgs(nil, CLISpec{ModelID: "unsupported", Binary: "other", SupportsRestrictedTools: true}, nil, nil, ToolPolicy{Constrained: true})
	require.ErrorContains(t, err, "has no supported native-tool restriction interface")
}

func TestToolPolicySnapshotIsImmutableOnWriteAndRead(t *testing.T) {
	allowed := []string{"view"}
	ctx := WithToolPolicy(context.Background(), ToolPolicy{Allowed: allowed, Constrained: true})
	allowed[0] = "bash"

	policy, ok := ToolPolicyFrom(ctx)
	require.True(t, ok)
	require.Equal(t, []string{"view"}, policy.Allowed)
	policy.Allowed[0] = "write"

	readAgain, ok := ToolPolicyFrom(ctx)
	require.True(t, ok)
	require.Equal(t, []string{"view"}, readAgain.Allowed)
}
