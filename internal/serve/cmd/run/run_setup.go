package run

import (
	"os"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/spf13/cobra"
)

func setupRunApp(cmd *cobra.Command, asJSON bool, sessionID string) (*app.App, error) {
	appOpts := cmdkit.McpAppOptions(cmd)
	if asJSON {
		appOpts = append(appOpts, app.WithBatchFaultJSONOutput(os.Stdout, sessionID))
	}
	return cmdkit.SetupAppWithOptions(cmd, appOpts)
}
