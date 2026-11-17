package run

import (
	"os"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/spf13/cobra"
)

func setupRunApp(cmd *cobra.Command, asJSON bool, sessionID string) (*app.App, error) {
	appOpts := cmdpkg.mcpAppOptions(cmd)
	if asJSON {
		appOpts = append(appOpts, app.WithBatchFaultJSONOutput(os.Stdout, sessionID))
	}
	return cmdpkg.setupAppWithOptions(cmd, appOpts)
}
