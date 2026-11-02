package cmd

import (
	"errors"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var apiCmd = newAPICmd()

func newAPICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "api",
		Short:   "Connect to a configured Rush API remote",
		Long:    "Manage configured Rush API remotes and use their REST API. This command does not initialize local application state.\n\nExit codes: 0 success, 1 failure, 2 usage/configuration error, 3 authentication/scope refused, 4 not found, 5 timeout, 6 paused, 130 interrupted.",
		Example: "rush api remote list",
		Args: func(cmd *cobra.Command, args []string) error {
			return rejectAPITokenArguments(cmd, args)
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if rootCmd.PersistentPreRunE != nil {
				if err := rootCmd.PersistentPreRunE(cmd, args); err != nil {
					return err
				}
			}
			return rejectAPITokenArguments(cmd, args)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	flags := cmd.PersistentFlags()
	flags.String("remote", "", "Configured API remote name")
	_ = cmd.RegisterFlagCompletionFunc("remote", completeAPIRemoteNames)
	flags.String("ca", "", "CA certificate file")
	flags.String("pin", "", "TLS certificate pin")
	flags.String("client-cert", "", "Client certificate file")
	flags.String("client-key", "", "Client private key file")
	flags.Bool("allow-cleartext", false, "Allow cleartext HTTP")
	flags.Bool("insecure-skip-verify", false, "Disable certificate verification for loopback only; warns on every invocation")
	flags.Bool("json", false, "Output JSON")
	flags.Bool("quiet", false, "Suppress non-error output")
	flags.Duration("timeout", 0, "Overall request timeout")
	cmd.AddCommand(newAPIRemoteCmd(), newAPISessionsCmd(), newAPIMessagesCmd(), newAPIModelsCmd(), newAPIOpenAPICmd(), newAPIRawCmd(), newAPIRunCmd(), newAPIRunsCmd(), newAPIEventsCmd(), newAPIInjectCmd(), newAPICancelRunCmd(), newAPIPermissionsCmd())
	return cmd
}

func rejectAPITokenArguments(cmd *cobra.Command, args []string) error {
	refuse := func() error {
		return &exitCodeError{code: 2, err: errors.New("token-looking input refused; rotate the token")}
	}
	for _, arg := range args {
		if config.LooksLikeAPIToken(arg) {
			return refuse()
		}
	}
	var found error
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		if found != nil {
			return
		}
		if config.LooksLikeAPIToken(flag.Value.String()) {
			found = refuse()
		}
	})
	return found
}

func apiFlagString(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return strings.TrimSpace(value)
}
