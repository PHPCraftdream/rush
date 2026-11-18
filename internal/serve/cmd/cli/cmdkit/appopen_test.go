package cmdkit

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestMcpAppOptions_InteractiveWebRootGetsNoRestriction pins that a
// command with no parent — true only for the bare `rush` invocation that
// starts the web UI, see runWebMode's own setupApp(cmd) call — never gets
// the RestrictMCPToCLI option, so every non-disabled MCP server keeps
// starting for interactive sessions exactly as before this feature.
func TestMcpAppOptions_InteractiveWebRootGetsNoRestriction(t *testing.T) {
	root := &cobra.Command{Use: "rush"}
	require.Nil(t, root.Parent())

	opts := McpAppOptions(root)
	require.Empty(t, opts, "the interactive web root command must not restrict MCP startup")
}

// TestMcpAppOptions_CLISubcommandGetsRestrictedByDefault pins the default
// for every OTHER command reachable from setupApp — `rush run`, `rush
// sessions ...`, `rush ping`, etc. — all of which have a non-nil parent.
func TestMcpAppOptions_CLISubcommandGetsRestrictedByDefault(t *testing.T) {
	root := &cobra.Command{Use: "rush"}
	sub := &cobra.Command{Use: "run"}
	root.AddCommand(sub)
	require.NotNil(t, sub.Parent(), "precondition: subcommand must have a parent")

	opts := McpAppOptions(sub)
	require.Len(t, opts, 1, "a CLI subcommand must restrict MCP startup to enabled_in_cli servers by default")
}

// TestMcpAppOptions_AllMCPFlagDisablesRestriction pins the --all-mcp
// escape hatch (run.go): when present and true, mcpAppOptions must return
// no restriction, exactly like the interactive web root.
func TestMcpAppOptions_AllMCPFlagDisablesRestriction(t *testing.T) {
	root := &cobra.Command{Use: "rush"}
	sub := &cobra.Command{Use: "run"}
	sub.Flags().Bool("all-mcp", false, "")
	root.AddCommand(sub)
	require.NoError(t, sub.Flags().Set("all-mcp", "true"))

	opts := McpAppOptions(sub)
	require.Empty(t, opts, "--all-mcp must disable the CLI restriction entirely")
}

// TestMcpAppOptions_MissingAllMCPFlagStillRestricts pins that commands
// which never register --all-mcp (every setupApp caller except `rush
// run`) are unaffected by its absence: GetBool's ignored error must not
// accidentally disable the restriction.
func TestMcpAppOptions_MissingAllMCPFlagStillRestricts(t *testing.T) {
	root := &cobra.Command{Use: "rush"}
	sub := &cobra.Command{Use: "sessions-list-stand-in"}
	root.AddCommand(sub)

	opts := McpAppOptions(sub)
	require.Len(t, opts, 1, "a command without --all-mcp registered must still restrict MCP startup")
}
