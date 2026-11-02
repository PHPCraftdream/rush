package cmd

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/apiclient"
	"github.com/spf13/cobra"
)

func newAPIInjectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "inject SESSION TEXT", Short: "Inject text into a remote session", Long: "Inject text into the active run in a remote session." + apiLeafHelp, Example: `rush api inject abc123 "Additional context"`, Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			if err := c.Inject(ctx, args[0], args[1], false); err != nil {
				return apiClientError(err)
			}
			if apiJSON(cmd) {
				return printAPIJSON(cmd, map[string]any{"session_id": args[0], "injected": true})
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "Injected")
			return err
		})
	}}
	return cmd
}

func newAPICancelRunCmd() *cobra.Command {
	return &cobra.Command{Use: "cancel RUN", Short: "Cancel a remote run", Long: "Request cancellation of a remote run. This is shorthand for `rush api runs cancel RUN`." + apiLeafHelp, Example: "rush api cancel run123", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			run, err := c.CancelRun(ctx, args[0])
			if err != nil {
				return apiClientError(err)
			}
			return printAPISingle(cmd, run)
		})
	}}
}
