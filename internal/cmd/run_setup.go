package cmd

import (
	"os"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/spf13/cobra"
)

func setupRunApp(cmd *cobra.Command, asJSON bool, sessionID string) (*app.App, error) {
	appOpts := mcpAppOptions(cmd)
	if asJSON {
		appOpts = append(appOpts, app.WithBatchFaultJSONOutput(os.Stdout, sessionID))
	}
	return setupAppWithOptions(cmd, appOpts)
}
