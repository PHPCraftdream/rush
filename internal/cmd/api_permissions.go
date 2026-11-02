package cmd

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/apiclient"
	"github.com/spf13/cobra"
)

func newAPIPermissionsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "permissions", Short: "Manage remote permissions", Long: "List pending requests and allow once or deny them." + apiLeafHelp, Example: "rush api permissions list", Args: cobra.NoArgs}
	cmd.AddCommand(newAPIPermissionsListCmd(), newAPIPermissionsDecisionCmd("allow", true), newAPIPermissionsDecisionCmd("deny", false))
	return cmd
}

func newAPIPermissionsListCmd() *cobra.Command {
	var session string
	cmd := &cobra.Command{Use: "list", Short: "List pending permissions", Long: "List pending tool permission requests, optionally filtered by session." + apiLeafHelp, Example: "rush api permissions list --session abc123", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			items, err := c.ListPermissions(ctx, apiclient.PermissionFilter{SessionID: session, State: "pending"})
			if err != nil {
				return apiClientError(err)
			}
			return printAPIItems(cmd, items, []string{"id", "session_id", "run_id", "tool", "description"})
		})
	}}
	cmd.Flags().StringVar(&session, "session", "", "Filter by session id")
	return cmd
}

func newAPIPermissionsDecisionCmd(action string, allow bool) *cobra.Command {
	return &cobra.Command{Use: action + " ID", Short: action + " a permission request", Long: fmt.Sprintf("%s a permission request %s; allow applies once only.", action, map[bool]string{true: "(once)", false: "(deny only)"}[allow]) + apiLeafHelp, Example: "rush api permissions " + action + " permission123", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			if err := c.AnswerPermission(ctx, args[0], allow); err != nil {
				return apiClientError(err)
			}
			if apiJSON(cmd) {
				return printAPIJSON(cmd, map[string]string{"id": args[0], "decision": action})
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", action, args[0])
			return err
		})
	}}
}
