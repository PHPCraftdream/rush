package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// A runnable leaf command without Args accepts any positional arguments and
// ignores them: `rush sessions cost <id>` printed the totals of every session
// as if they were that session's. Every runnable leaf must declare its
// arguments.
func TestRunnableLeafCommandsDeclareArgs(t *testing.T) {
	var missing []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Runnable() && !c.HasSubCommands() && c.Args == nil {
			missing = append(missing, c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
	require.Empty(t, missing, "runnable commands without an Args validator")
}

func TestSessionsCostRejectsPositionalArgs(t *testing.T) {
	require.Error(t, sessionsCostCmd.Args(sessionsCostCmd, []string{"some-session"}))
	require.NoError(t, sessionsCostCmd.Args(sessionsCostCmd, nil))
}
