// Package apisessions builds the session commands and owns the shared API
// client, flag, and output helpers. It does not depend on the other API commands.
package apisessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/client/apiclient"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	"github.com/spf13/cobra"
)

const ApiPageCap = 10000

const ApiLeafHelp = "\n\nExit codes: 0 success, 1 network/TLS/server error, 2 usage/configuration error, 3 authentication or scope refused, 4 not found, 5 timeout, 6 paused, 130 interrupted."

func NewAPISessionsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sessions", Short: "Manage remote API sessions", Long: "List and manage sessions on the selected API remote." + ApiLeafHelp, Example: "rush api sessions list", Args: cobra.NoArgs}
	cmd.AddCommand(newAPISessionsListCmd(), newAPISessionsGetCmd(), newAPISessionsCreateCmd(), newAPISessionsForkCmd(), newAPISessionsDeleteCmd())
	return cmd
}

func newAPISessionsListCmd() *cobra.Command {
	var after string
	var limit int
	var all bool
	cmd := &cobra.Command{Use: "list", Short: "List remote sessions", Long: "List remote sessions. --all follows pagination up to a hard cap of 10000 sessions." + ApiLeafHelp, Example: "rush api sessions list --limit 50", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := validateAPIPageFlags(after, limit, all); err != nil {
			return ApiConfigurationError(err)
		}
		return ApiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			if all {
				items, err := c.AllSessions(ctx, ApiPageCap)
				if err != nil {
					return ApiClientError(err)
				}
				return PrintAPIItems(cmd, items, sessionColumns)
			}
			page, err := c.ListSessions(ctx, apiclient.SessionsOptions{After: after, Limit: limit})
			if err != nil {
				return ApiClientError(err)
			}
			return PrintAPIItems(cmd, page.Items, sessionColumns)
		})
	}}
	cmd.Flags().StringVar(&after, "after", "", "Return items after this cursor")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum items to return (1-10000)")
	cmd.Flags().BoolVar(&all, "all", false, "Fetch all pages, up to 10000 items")
	return cmd
}

func newAPISessionsGetCmd() *cobra.Command {
	return &cobra.Command{Use: "get ID", Short: "Get a remote session", Long: "Get one remote session by ID." + ApiLeafHelp, Example: "rush api sessions get abc123", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return ApiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			item, err := c.GetSession(ctx, args[0])
			if err != nil {
				return ApiClientError(err)
			}
			return PrintAPISingle(cmd, item)
		})
	}}
}

func newAPISessionsCreateCmd() *cobra.Command {
	var title string
	cmd := &cobra.Command{Use: "create", Short: "Create a remote session", Long: "Create a remote session." + ApiLeafHelp, Example: "rush api sessions create --title review", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return ApiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			item, err := c.CreateSession(ctx, title)
			if err != nil {
				return ApiClientError(err)
			}
			return PrintAPISingle(cmd, item)
		})
	}}
	cmd.Flags().StringVar(&title, "title", "", "Session title")
	return cmd
}

func newAPISessionsForkCmd() *cobra.Command {
	var title string
	cmd := &cobra.Command{Use: "fork ID", Short: "Fork a remote session", Long: "Fork a remote session." + ApiLeafHelp, Example: "rush api sessions fork abc123 --title experiment", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return ApiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			item, err := c.ForkSession(ctx, args[0], title)
			if err != nil {
				return ApiClientError(err)
			}
			return PrintAPISingle(cmd, item)
		})
	}}
	cmd.Flags().StringVar(&title, "title", "", "Forked session title")
	return cmd
}

func newAPISessionsDeleteCmd() *cobra.Command {
	return &cobra.Command{Use: "delete ID", Short: "Delete a remote session", Long: "Delete a remote session by ID." + ApiLeafHelp, Example: "rush api sessions delete abc123", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return ApiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			if err := c.DeleteSession(ctx, args[0]); err != nil {
				return ApiClientError(err)
			}
			if !ApiQuiet(cmd) {
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted session %s\n", args[0])
			}
			return nil
		})
	}}
}

func validateAPIPageFlags(after string, limit int, all bool) error {
	if limit < 0 || limit > ApiPageCap {
		return errors.New("--limit must be between 0 and 10000")
	}
	if all && after != "" {
		return errors.New("--after cannot be combined with --all")
	}
	return nil
}

func ApiWithClient(cmd *cobra.Command, action func(*apiclient.Client, context.Context) error) error {
	ctx, cancel, err := ApiCommandContext(cmd, 60*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	client, err := NewAPIClient(cmd)
	if err != nil {
		return err
	}
	return action(client, ctx)
}

func ApiClientError(err error) error {
	return cliout.NewExitCodeError(apiclient.ExitCodeFor(err), err)
}

func NewAPIMessagesCmd() *cobra.Command {
	var after string
	var limit int
	var all bool
	cmd := &cobra.Command{Use: "messages SESSION", Short: "List messages in a remote session", Long: "List messages in a remote session. --all follows pages up to the 10000-item cap." + ApiLeafHelp, Example: "rush api messages abc123 --limit 50", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAPIPageFlags(after, limit, all); err != nil {
			return ApiConfigurationError(err)
		}
		return ApiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			if all {
				items, err := c.AllMessages(ctx, args[0], ApiPageCap)
				if err != nil {
					return ApiClientError(err)
				}
				return PrintAPIItems(cmd, items, messageColumns)
			}
			page, err := c.ListMessages(ctx, args[0], after, limit)
			if err != nil {
				return ApiClientError(err)
			}
			return PrintAPIItems(cmd, page.Items, messageColumns)
		})
	}}
	cmd.Flags().StringVar(&after, "after", "", "Return items after this cursor")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum items to return (1-10000)")
	cmd.Flags().BoolVar(&all, "all", false, "Fetch all pages, up to 10000 items")
	return cmd
}

func NewAPIModelsCmd() *cobra.Command {
	return &cobra.Command{Use: "models", Short: "List remote API models", Long: "List models available from the selected remote." + ApiLeafHelp, Example: "rush api models", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return ApiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			items, err := c.Models(ctx)
			if err != nil {
				return ApiClientError(err)
			}
			return PrintAPIItems(cmd, items, modelColumns)
		})
	}}
}

var (
	sessionColumns = []string{"id", "title", "created_at", "message_count"}
	messageColumns = []string{"id", "role", "created_at"}
	modelColumns   = []string{"id", "name", "provider"}
)

func PrintAPIItems[T any](cmd *cobra.Command, items []T, columns []string) error {
	if ApiJSON(cmd) {
		for _, item := range items {
			if err := PrintAPIJSON(cmd, item); err != nil {
				return err
			}
		}
		return nil
	}
	writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, strings.ToUpper(strings.Join(columns, "	")))
	for _, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			return ApiConfigurationError(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			return ApiConfigurationError(err)
		}
		cells := make([]string, len(columns))
		for i, column := range columns {
			cells[i] = "-"
			if value, ok := fields[column]; ok && value != nil && fmt.Sprint(value) != "" {
				cells[i] = fmt.Sprint(value)
			}
		}
		fmt.Fprintln(writer, strings.Join(cells, "	"))
	}
	return writer.Flush()
}

func PrintAPISingle(cmd *cobra.Command, item any) error {
	if ApiJSON(cmd) {
		return PrintAPIJSON(cmd, item)
	}
	data, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return ApiConfigurationError(err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return err
}

func PrintAPIJSON(cmd *cobra.Command, item any) error {
	data, err := json.Marshal(item)
	if err != nil {
		return ApiConfigurationError(err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return err
}

func NewAPIClient(cmd *cobra.Command) (*apiclient.Client, error) {
	_, remote, err := config.ResolveAPIRemote(ApiFlagString(cmd, "remote"), os.Getenv)
	if err != nil {
		return nil, ApiConfigurationError(err)
	}
	token, err := config.ReadAPIRemoteToken(remote, os.Getenv)
	if err != nil {
		return nil, ApiConfigurationError(err)
	}
	ca := remote.CA
	if cmd.Flags().Changed("ca") {
		ca = ApiFlagString(cmd, "ca")
	}
	pin := remote.Pin
	if cmd.Flags().Changed("pin") {
		pin = ApiFlagString(cmd, "pin")
	}
	cert := remote.ClientCert
	if cmd.Flags().Changed("client-cert") {
		cert = ApiFlagString(cmd, "client-cert")
	}
	key := remote.ClientKey
	if cmd.Flags().Changed("client-key") {
		key = ApiFlagString(cmd, "client-key")
	}
	allowCleartext := remote.AllowCleartext
	if cmd.Flags().Changed("allow-cleartext") {
		allowCleartext, _ = cmd.Flags().GetBool("allow-cleartext")
	}
	insecure, _ := cmd.Flags().GetBool("insecure-skip-verify")
	if _, err := apiTimeout(cmd); err != nil {
		return nil, err
	}
	options := apiclient.Options{
		BaseURL: remote.URL, Token: token, CAFile: ca, Pin: pin,
		ClientCertFile: cert, ClientKeyFile: key,
		AllowCleartext: allowCleartext, InsecureSkipVerify: insecure,
	}
	client, warnings, err := apiclient.New(options)
	for _, warning := range warnings {
		fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: "+warning)
	}
	if err != nil {
		return nil, cliout.NewExitCodeError(apiclient.ExitCodeFor(err), err)
	}
	return client, nil
}

func ApiConfigurationError(err error) error {
	return cliout.NewExitCodeError(2, err)
}

// ApiCommandContext bounds a complete API operation, not merely its HTTP
// response-header wait. Simple REST leaf commands should pass 60*time.Second;
// streaming and wait commands should pass 0 unless --timeout was provided.
// A leaf must use the returned context for its client call and defer cancel.
func ApiCommandContext(cmd *cobra.Command, defaultTimeout time.Duration) (context.Context, context.CancelFunc, error) {
	timeout, err := apiTimeout(cmd)
	if err != nil {
		return nil, nil, err
	}
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout > 0 {
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		return ctx, cancel, nil
	}
	return cmd.Context(), func() {}, nil
}

func apiTimeout(cmd *cobra.Command) (time.Duration, error) {
	duration, err := cmd.Flags().GetDuration("timeout")
	if err != nil || duration < 0 {
		if err == nil {
			err = errors.New("--timeout must not be negative")
		}
		return 0, cliout.NewExitCodeError(2, err)
	}
	return duration, nil
}

func ApiFlagString(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return strings.TrimSpace(value)
}

func ApiJSON(cmd *cobra.Command) bool {
	value, err := cmd.Flags().GetBool("json")
	return err == nil && value
}

func ApiQuiet(cmd *cobra.Command) bool {
	value, err := cmd.Flags().GetBool("quiet")
	return err == nil && value
}

func InterruptedAPIError() error { return cliout.NewExitCodeError(130, fmt.Errorf("interrupted")) }
