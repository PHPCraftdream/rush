package apictl

import (
	remotectl "github.com/PHPCraftdream/rush/internal/serve/cmd/admin/serverctl/api/remote"
	apiruns "github.com/PHPCraftdream/rush/internal/serve/cmd/admin/serverctl/api/runs"
	apisessions "github.com/PHPCraftdream/rush/internal/serve/cmd/admin/serverctl/api/sessions"
	"github.com/spf13/cobra"
)

var apiCmd = newAPICmd()

func newAPICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "api",
		Short:   "Connect to a configured Rush API remote",
		Long:    "Manage configured Rush API remotes and use their REST API. This command does not initialize local application state.\n\nExit codes: 0 success, 1 failure, 2 usage/configuration error, 3 authentication/scope refused, 4 not found, 5 timeout, 6 paused, 130 interrupted.",
		Example: "rush api remote list",
		Args: func(cmd *cobra.Command, args []string) error {
			return remotectl.RejectAPITokenArguments(cmd, args)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	flags := cmd.PersistentFlags()
	flags.String("remote", "", "Configured API remote name")
	_ = cmd.RegisterFlagCompletionFunc("remote", remotectl.CompleteAPIRemoteNames)
	flags.String("ca", "", "CA certificate file")
	flags.String("pin", "", "TLS certificate pin")
	flags.String("client-cert", "", "Client certificate file")
	flags.String("client-key", "", "Client private key file")
	flags.Bool("allow-cleartext", false, "Allow cleartext HTTP")
	flags.Bool("insecure-skip-verify", false, "Disable certificate verification for loopback only; warns on every invocation")
	flags.Bool("json", false, "Output JSON")
	flags.Bool("quiet", false, "Suppress non-error output")
	flags.Duration("timeout", 0, "Overall request timeout")
	cmd.AddCommand(remotectl.NewAPIRemoteCmd(), apisessions.NewAPISessionsCmd(), apisessions.NewAPIMessagesCmd(), apisessions.NewAPIModelsCmd(), newAPIOpenAPICmd(), newAPIRawCmd(), apiruns.NewAPIRunCmd(), apiruns.NewAPITaskCmd(), apiruns.NewAPIBatchCmd(), apiruns.NewAPIRunsCmd(), remotectl.NewAPIEventsCmd(), apisessions.NewAPIInjectCmd(), apisessions.NewAPICancelRunCmd(), remotectl.NewAPIPermissionsCmd())
	return cmd
}

// NewAPICmd returns a fresh rush api command tree.
func NewAPICmd() *cobra.Command {
	return newAPICmd()
}
