package modelopts

import (
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	journal "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/journal"
	"github.com/spf13/cobra"
)

var modelsSetCmd = &cobra.Command{
	Use:                "set",
	Args:               cobra.NoArgs,
	Hidden:             true,
	Short:              "(removed — use `rush models use`)",
	DisableFlagParsing: true, // print the redirect even when caller passes --large/--small/etc.
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(os.Stderr,
			"`rush models set` was removed in batch 11.\n"+
				"Use `rush models use <smart> <fast>` instead (add --worker/--reviewer to set\n"+
				"those optional slots too). See `rush models list` for atoms.")
		journal.ExitWithAudit(2)
	},
}

// NewSetCmd returns a fresh command.
func NewSetCmd() *cobra.Command {
	return cmdkit.DeepFreshCmd(modelsSetCmd)
}
