package cmd

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/apiclient"
	"github.com/spf13/cobra"
)

func newAPIRunsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "runs", Short: "Manage remote runs", Long: "Inspect, wait for, or cancel remote runs." + apiLeafHelp, Example: "rush api runs get run123", Args: cobra.NoArgs}
	cmd.AddCommand(newAPIRunsGetCmd(), newAPIRunsWaitCmd(), newAPIRunsCancelCmd())
	return cmd
}

func newAPIRunsGetCmd() *cobra.Command {
	return &cobra.Command{Use: "get RUN", Short: "Get a remote run", Long: "Get the current state of a remote run." + apiLeafHelp, Example: "rush api runs get run123", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			run, err := c.GetRun(ctx, args[0], 0)
			if err != nil {
				return apiClientError(err)
			}
			return printAPISingle(cmd, run)
		})
	}}
}

func newAPIRunsWaitCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "wait RUN", Short: "Wait for a remote run", Long: "Wait for a remote run to finish or pause." + apiLeafHelp, Example: "rush api runs wait run123 --timeout 2m", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel, err := apiCommandContext(cmd, 0)
		if err != nil {
			return err
		}
		defer cancel()
		return runAPIWithContext(cmd, ctx, func(c *apiclient.Client) error {
			run, err := waitAPIRun(ctx, c, args[0])
			if err != nil {
				return apiClientError(err)
			}
			return printAPISingle(cmd, run)
		})
	}}
	return cmd
}

func newAPIRunsCancelCmd() *cobra.Command {
	return &cobra.Command{Use: "cancel RUN", Short: "Cancel a remote run", Long: "Request cancellation of a remote run." + apiLeafHelp, Example: "rush api runs cancel run123", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			run, err := c.CancelRun(ctx, args[0])
			if err != nil {
				return apiClientError(err)
			}
			return printAPISingle(cmd, run)
		})
	}}
}

func runAPIWithContext(cmd *cobra.Command, ctx context.Context, action func(*apiclient.Client) error) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	return action(client)
}
