package cli

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// A runnable leaf command without Args accepts any positional arguments and
// ignores them: every leaf must declare its argument contract.
func TestRunnableLeafCommandsDeclareArgs(t *testing.T) {
	var missing []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Runnable() && !c.HasSubCommands() && c.Args == nil {
			missing = append(missing, c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(NewRootCmd())
	require.Empty(t, missing, "runnable commands without an Args validator")
}
