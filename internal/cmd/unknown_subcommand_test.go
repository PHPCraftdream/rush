package cmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func newUnknownSubcommandTree() *cobra.Command {
	root := &cobra.Command{Use: "root", SilenceUsage: true, SilenceErrors: true}
	group := &cobra.Command{Use: "group", Short: "a group"}
	group.AddCommand(&cobra.Command{Use: "show", Run: func(*cobra.Command, []string) {}})
	root.AddCommand(group)
	guardUnknownSubcommands(root)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	return root
}

func TestGuardUnknownSubcommandFails(t *testing.T) {
	root := newUnknownSubcommandTree()
	root.SetArgs([]string{"group", "bogus"})
	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), `unknown command "bogus" for "root group"`)
}

func TestGuardUnknownSubcommandSuggests(t *testing.T) {
	root := newUnknownSubcommandTree()
	root.SetArgs([]string{"group", "shwo"})
	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "Did you mean this?\n\tshow")
}

func TestGuardUnknownSubcommandBareGroupShowsHelp(t *testing.T) {
	root := newUnknownSubcommandTree()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"group"})
	require.NoError(t, root.Execute())
	require.Contains(t, out.String(), "show")
}

// Every group in the real tree must reject unknown subcommands, so a group
// added later cannot reintroduce the help-and-exit-0 behavior.
func TestGuardUnknownSubcommandCoversRealTree(t *testing.T) {
	guardUnknownSubcommands(rootCmd)
	// rootCmd is shared with the other tests: undo the guard afterwards.
	t.Cleanup(func() { unguardUnknownSubcommands(rootCmd) })
	guarded := map[string]bool{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, child := range c.Commands() {
			walk(child)
		}
		if !c.HasParent() || !c.HasSubCommands() {
			return
		}
		// A group with its own Run handles its arguments itself.
		require.True(t, c.Runnable(), "%s must be runnable", c.CommandPath())
		if c.Annotations[unknownSubcommandGuard] == "" {
			return
		}
		guarded[c.Name()] = true
		err := c.RunE(c, []string{"definitely-not-a-subcommand"})
		require.Error(t, err, c.CommandPath())
		require.Contains(t, err.Error(), "unknown command", c.CommandPath())
	}
	walk(rootCmd)
	require.True(t, guarded["sessions"], "rush sessions must be guarded")
	require.True(t, guarded["batch"], "rush batch must be guarded")
}

func unguardUnknownSubcommands(cmd *cobra.Command) {
	for _, c := range cmd.Commands() {
		unguardUnknownSubcommands(c)
	}
	if cmd.Annotations[unknownSubcommandGuard] == "" {
		return
	}
	delete(cmd.Annotations, unknownSubcommandGuard)
	cmd.Args = nil
	cmd.RunE = nil
}
