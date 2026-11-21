package modelopts

import (
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	journal "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/journal"
	"github.com/spf13/cobra"
)

var modelsPresetCmd = &cobra.Command{
	Use:                "preset",
	Args:               cobra.NoArgs,
	Hidden:             true,
	Short:              "(removed — use `rush models use` / `rush models list`)",
	DisableFlagParsing: true, // print the redirect even when caller passes legacy preset args.
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(os.Stderr,
			"`rush models preset` was removed in batch 11.\n"+
				"Use `rush models list` to see atoms, then `rush models use <smart> <fast>`.")
		journal.ExitWithAudit(2)
	},
}

// NewPresetCmd returns a fresh command.
func NewPresetCmd() *cobra.Command {
	return cmdkit.DeepFreshCmd(modelsPresetCmd)
}
