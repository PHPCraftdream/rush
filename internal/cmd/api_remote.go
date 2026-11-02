package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/apiclient"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const apiRemoteHelp = "\n\nExit codes: 0 success, 1 network/TLS/server or I/O failure, 2 usage/configuration error, 3 authentication or scope refused, 4 remote not found, 5 timeout, 130 interrupted."

func newAPIRemoteCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "remote", Short: "Manage API remote profiles", Long: "Add, inspect, test, and remove global API remote profiles." + apiRemoteHelp, Example: "rush api remote list", Args: cobra.NoArgs}
	cmd.AddCommand(newAPIRemoteAddCmd(), newAPIRemoteListCmd(), newAPIRemoteShowCmd(), newAPIRemoteRemoveCmd(), newAPIRemoteTestCmd())
	return cmd
}

func newAPIRemoteAddCmd() *cobra.Command {
	var url, tokenEnv, tokenFile string
	var tokenStdin, allowCleartext, setDefault, replace bool
	var ca, pin, clientCert, clientKey string
	cmd := &cobra.Command{
		Use: "add NAME", Short: "Add an API remote profile",
		Long:    "Store a global remote profile. Exactly one token source is required; --token-stdin stores the entered secret in the protected profile token file. Secrets are never displayed." + apiRemoteHelp,
		Example: "rush api remote add prod --url https://api.example --token-env RUSH_PROD_TOKEN --default",
		Args: func(_ *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(1)(nil, args); err != nil {
				return apiConfigurationError(err)
			}
			selectedSources := boolInt(tokenEnv != "") + boolInt(tokenFile != "") + boolInt(tokenStdin)
			if selectedSources != 1 {
				return apiConfigurationError(errors.New("choose exactly one of --token-env, --token-file, or --token-stdin"))
			}
			if strings.TrimSpace(url) == "" {
				return apiConfigurationError(errors.New("--url is required"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectAPITokenArguments(cmd, args); err != nil {
				return err
			}
			profile := config.APIRemote{URL: url, TokenEnv: tokenEnv, TokenFile: tokenFile, CA: ca, Pin: pin, ClientCert: clientCert, ClientKey: clientKey, AllowCleartext: allowCleartext}
			if !replace {
				if _, err := config.GetAPIRemote(args[0]); err == nil {
					return apiConfigurationError(errors.New("API remote exists; explicit --replace required"))
				} else if !strings.Contains(err.Error(), "not found") {
					return apiConfigurationError(err)
				}
			}
			if tokenStdin {
				validation := profile
				validation.TokenEnv = "RUSH_API_VALIDATION"
				validation.TokenFile = ""
				if err := config.ValidateAPIRemote(validation); err != nil {
					return apiConfigurationError(err)
				}
				secret, err := readAPIRemoteToken(cmd)
				if err != nil {
					return apiConfigurationError(err)
				}
				if !config.LooksLikeAPIToken(string(secret)) {
					return apiConfigurationError(errors.New("token input is not a valid API token; rotate the token if it was exposed"))
				}
				path, err := config.WriteAPIRemoteTokenFile(args[0], secret)
				for i := range secret {
					secret[i] = 0
				}
				if err != nil {
					return apiConfigurationError(err)
				}
				profile.TokenFile = path
			}
			if err := config.SetAPIRemote(args[0], profile, replace); err != nil {
				if tokenStdin && !replace {
					_ = os.Remove(profile.TokenFile)
				}
				return apiConfigurationError(err)
			}
			if setDefault {
				if err := config.SetDefaultAPIRemote(args[0]); err != nil {
					return apiConfigurationError(err)
				}
			}
			if !apiQuiet(cmd) {
				fmt.Fprintf(cmd.OutOrStdout(), "Added remote %s\n", args[0])
			}
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&url, "url", "", "API base URL")
	flags.StringVar(&tokenEnv, "token-env", "", "Environment variable containing the API token")
	flags.StringVar(&tokenFile, "token-file", "", "Absolute or home-relative API token file")
	flags.BoolVar(&tokenStdin, "token-stdin", false, "Read the API token from stdin or a hidden terminal prompt")
	flags.StringVar(&ca, "ca", "", "CA certificate file")
	flags.StringVar(&pin, "pin", "", "TLS certificate pin")
	flags.StringVar(&clientCert, "client-cert", "", "Client certificate file")
	flags.StringVar(&clientKey, "client-key", "", "Client private key file")
	flags.BoolVar(&allowCleartext, "allow-cleartext", false, "Allow cleartext HTTP")
	flags.BoolVar(&setDefault, "default", false, "Set this profile as the default remote")
	flags.BoolVar(&replace, "replace", false, "Replace an existing profile")
	return cmd
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func readAPIRemoteToken(cmd *cobra.Command) ([]byte, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(cmd.ErrOrStderr(), "API token: ")
		secret, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		return secret, err
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return []byte(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")), nil
}

func newAPIRemoteListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List API remote profiles", Long: "List global remote profile metadata; credentials are not shown." + apiRemoteHelp, Example: "rush api remote list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		profiles, defaultName, err := config.ListAPIRemotes()
		if err != nil {
			return apiConfigurationError(err)
		}
		type row struct {
			Name       string `json:"name"`
			URL        string `json:"url"`
			Credential string `json:"credential_source"`
			Default    bool   `json:"default"`
		}
		names := sortedRemoteNames(profiles)
		writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "NAME\tURL\tCREDENTIAL\tDEFAULT")
		for _, name := range names {
			remote := profiles[name]
			source := remoteCredentialSource(remote)
			item := row{Name: name, URL: remote.URL, Credential: source, Default: name == defaultName}
			if apiJSON(cmd) {
				data, err := json.Marshal(item)
				if err != nil {
					return apiConfigurationError(err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(data))
			} else {
				fmt.Fprintf(writer, "%s\t%s\t%s\t%t\n", item.Name, item.URL, item.Credential, item.Default)
			}
		}
		if !apiJSON(cmd) {
			return writer.Flush()
		}
		return nil
	}}
}

func newAPIRemoteShowCmd() *cobra.Command {
	return &cobra.Command{Use: "show NAME", Short: "Show an API remote profile", Long: "Show remote settings and credential source only; never print the credential." + apiRemoteHelp, Example: "rush api remote show prod", Args: func(_ *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(1)(nil, args); err != nil {
			return apiConfigurationError(err)
		}
		return nil
	}, ValidArgsFunction: completeAPIRemoteNames, RunE: func(cmd *cobra.Command, args []string) error {
		remote, err := config.GetAPIRemote(args[0])
		if err != nil {
			return apiRemoteConfigError(err)
		}
		_, def, err := config.ListAPIRemotes()
		if err != nil {
			return apiConfigurationError(err)
		}
		result := map[string]any{"name": args[0], "url": remote.URL, "credential_source": remoteCredentialSource(remote), "ca": remote.CA, "pin": remote.Pin, "client_cert": remote.ClientCert, "client_key": remote.ClientKey, "allow_cleartext": remote.AllowCleartext, "default": args[0] == def}
		if apiJSON(cmd) {
			data, err := json.Marshal(result)
			if err != nil {
				return apiConfigurationError(err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Name: %s\nURL: %s\nCredential source: %s\nCA: %s\nPin: %s\nClient certificate: %s\nClient key: %s\nAllow cleartext: %t\nDefault: %t\n", args[0], remote.URL, remoteCredentialSource(remote), remote.CA, remote.Pin, remote.ClientCert, remote.ClientKey, remote.AllowCleartext, args[0] == def)
		}
		return nil
	}}
}

func newAPIRemoteRemoveCmd() *cobra.Command {
	return &cobra.Command{Use: "rm NAME", Short: "Remove an API remote profile", Long: "Remove a global API remote profile and its managed token file." + apiRemoteHelp, Example: "rush api remote rm prod", Args: func(_ *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(1)(nil, args); err != nil {
			return apiConfigurationError(err)
		}
		return nil
	}, ValidArgsFunction: completeAPIRemoteNames, RunE: func(cmd *cobra.Command, args []string) error {
		if err := config.RemoveAPIRemote(args[0]); err != nil {
			return apiRemoteConfigError(err)
		}
		if !apiQuiet(cmd) {
			fmt.Fprintf(cmd.OutOrStdout(), "Removed remote %s\n", args[0])
		}
		return nil
	}}
}

func newAPIRemoteTestCmd() *cobra.Command {
	return &cobra.Command{Use: "test [NAME]", Short: "Test an API remote connection", Long: "Check the selected API remote with GET /health and GET /version; report the server version and configured TLS facts." + apiRemoteHelp, Example: "rush api remote test prod", Args: func(_ *cobra.Command, args []string) error {
		if len(args) > 1 {
			return apiConfigurationError(errors.New("accepts at most one remote name"))
		}
		return nil
	}, ValidArgsFunction: completeAPIRemoteNames, RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			if err := cmd.Flags().Set("remote", args[0]); err != nil {
				return apiConfigurationError(err)
			}
		}
		ctx, cancel, err := apiCommandContext(cmd, 60*time.Second)
		if err != nil {
			return err
		}
		defer cancel()
		cmd.SetContext(ctx)
		client, err := newAPIClient(cmd)
		if err != nil {
			return err
		}
		if err = client.Health(ctx); err != nil {
			return &exitCodeError{code: apiclient.ExitCodeFor(err), err: err}
		}
		version, err := client.Version(ctx)
		if err != nil {
			return &exitCodeError{code: apiclient.ExitCodeFor(err), err: err}
		}
		name, remote, err := config.ResolveAPIRemote(apiFlagString(cmd, "remote"), os.Getenv)
		if err != nil {
			return apiConfigurationError(err)
		}
		if !apiQuiet(cmd) {
			fmt.Fprintf(cmd.OutOrStdout(), "Remote %s: healthy\nServer version: %s\nTLS: %s\nCertificate pin: %s\n", name, version, remoteTLSFact(remote.URL), pinFact(remote.Pin))
		}
		return nil
	}}
}

func completeAPIRemoteNames(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	profiles, _, err := config.ListAPIRemotes()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return sortedRemoteNames(profiles), cobra.ShellCompDirectiveNoFileComp
}

func sortedRemoteNames(profiles map[string]config.APIRemote) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func remoteCredentialSource(remote config.APIRemote) string {
	if remote.TokenEnv != "" {
		return "environment:" + remote.TokenEnv
	}
	if remote.TokenFile != "" {
		return "file"
	}
	return "none"
}
func remoteTLSFact(raw string) string {
	if strings.HasPrefix(raw, "https://") {
		return "HTTPS certificate verification enabled (unless --insecure-skip-verify is used)"
	}
	return "HTTP cleartext"
}
func pinFact(pin string) string {
	if pin == "" {
		return "not configured"
	}
	return "configured"
}
func apiRemoteConfigError(err error) error {
	if strings.Contains(err.Error(), "not found") {
		return &exitCodeError{code: 4, err: err}
	}
	return apiConfigurationError(err)
}
func apiJSON(cmd *cobra.Command) bool {
	value, err := cmd.Flags().GetBool("json")
	return err == nil && value
}
func apiQuiet(cmd *cobra.Command) bool {
	value, err := cmd.Flags().GetBool("quiet")
	return err == nil && value
}
