package apictl

import (
	remotectl "github.com/PHPCraftdream/rush/internal/serve/cmd/admin/serverctl/api/remote"
	"github.com/spf13/cobra"
)

// APITokenArgumentGuard is called by the cli root pre-run (a subcommand must
// not set its own PersistentPreRunE: cobra would run only the nearest one and
// skip the journal and password checks). It refuses token-shaped arguments and
// flag values for every command of the `rush api` group. root is the live
// tree root passed by cli.NewRootCmd (lane K2 crossing, design §3.4: the
// former rootCmd package global no longer exists).
func APITokenArgumentGuard(root *cobra.Command, cmd *cobra.Command, args []string) error {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Name() == "api" && (c.Parent() == nil || c.Parent() == root) {
			return remotectl.RejectAPITokenArguments(cmd, args)
		}
	}
	return nil
}
