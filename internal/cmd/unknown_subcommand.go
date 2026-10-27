package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

const unknownSubcommandGuard = "rush.unknown-subcommand-guard"

// guardUnknownSubcommands makes every command group reject an unknown
// subcommand with an error. Cobra only checks unknown commands on the root;
// a non-runnable group otherwise prints its help and exits 0, so a typo such
// as `rush sessions lsit` silently succeeds.
func guardUnknownSubcommands(cmd *cobra.Command) {
	for _, c := range cmd.Commands() {
		guardUnknownSubcommands(c)
	}
	if !cmd.HasParent() || !cmd.HasSubCommands() || cmd.Runnable() {
		return
	}
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[unknownSubcommandGuard] = "true"
	cmd.Args = cobra.ArbitraryArgs
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			return c.Help()
		}
		return unknownSubcommandError(c, args[0])
	}
}

func unknownSubcommandError(cmd *cobra.Command, name string) error {
	msg := fmt.Sprintf("unknown command %q for %q", name, cmd.CommandPath())
	// Cobra applies its default distance (2) only on the root.
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	if suggestions := cmd.SuggestionsFor(name); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t")
	}
	return errors.New(msg)
}
