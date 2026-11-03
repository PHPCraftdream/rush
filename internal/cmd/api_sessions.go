package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/apiclient"
	"github.com/spf13/cobra"
)

const apiPageCap = 10000

const apiLeafHelp = "\n\nExit codes: 0 success, 1 network/TLS/server error, 2 usage/configuration error, 3 authentication or scope refused, 4 not found, 5 timeout, 6 paused, 130 interrupted."

func newAPISessionsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sessions", Short: "Manage remote API sessions", Long: "List and manage sessions on the selected API remote." + apiLeafHelp, Example: "rush api sessions list", Args: cobra.NoArgs}
	cmd.AddCommand(newAPISessionsListCmd(), newAPISessionsGetCmd(), newAPISessionsCreateCmd(), newAPISessionsForkCmd(), newAPISessionsDeleteCmd())
	return cmd
}

func newAPISessionsListCmd() *cobra.Command {
	var after string
	var limit int
	var all bool
	cmd := &cobra.Command{Use: "list", Short: "List remote sessions", Long: "List remote sessions. --all follows pagination up to a hard cap of 10000 sessions." + apiLeafHelp, Example: "rush api sessions list --limit 50", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := validateAPIPageFlags(after, limit, all); err != nil {
			return apiConfigurationError(err)
		}
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			if all {
				items, err := c.AllSessions(ctx, apiPageCap)
				if err != nil {
					return apiClientError(err)
				}
				return printAPIItems(cmd, items, sessionColumns)
			}
			page, err := c.ListSessions(ctx, apiclient.SessionsOptions{After: after, Limit: limit})
			if err != nil {
				return apiClientError(err)
			}
			return printAPIItems(cmd, page.Items, sessionColumns)
		})
	}}
	cmd.Flags().StringVar(&after, "after", "", "Return items after this cursor")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum items to return (1-10000)")
	cmd.Flags().BoolVar(&all, "all", false, "Fetch all pages, up to 10000 items")
	return cmd
}

func newAPISessionsGetCmd() *cobra.Command {
	return &cobra.Command{Use: "get ID", Short: "Get a remote session", Long: "Get one remote session by ID." + apiLeafHelp, Example: "rush api sessions get abc123", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			item, err := c.GetSession(ctx, args[0])
			if err != nil {
				return apiClientError(err)
			}
			return printAPISingle(cmd, item)
		})
	}}
}

func newAPISessionsCreateCmd() *cobra.Command {
	var title string
	cmd := &cobra.Command{Use: "create", Short: "Create a remote session", Long: "Create a remote session." + apiLeafHelp, Example: "rush api sessions create --title review", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			item, err := c.CreateSession(ctx, title)
			if err != nil {
				return apiClientError(err)
			}
			return printAPISingle(cmd, item)
		})
	}}
	cmd.Flags().StringVar(&title, "title", "", "Session title")
	return cmd
}

func newAPISessionsForkCmd() *cobra.Command {
	var title string
	cmd := &cobra.Command{Use: "fork ID", Short: "Fork a remote session", Long: "Fork a remote session." + apiLeafHelp, Example: "rush api sessions fork abc123 --title experiment", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			item, err := c.ForkSession(ctx, args[0], title)
			if err != nil {
				return apiClientError(err)
			}
			return printAPISingle(cmd, item)
		})
	}}
	cmd.Flags().StringVar(&title, "title", "", "Forked session title")
	return cmd
}

func newAPISessionsDeleteCmd() *cobra.Command {
	return &cobra.Command{Use: "delete ID", Short: "Delete a remote session", Long: "Delete a remote session by ID." + apiLeafHelp, Example: "rush api sessions delete abc123", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			if err := c.DeleteSession(ctx, args[0]); err != nil {
				return apiClientError(err)
			}
			if !apiQuiet(cmd) {
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted session %s\n", args[0])
			}
			return nil
		})
	}}
}

func validateAPIPageFlags(after string, limit int, all bool) error {
	if limit < 0 || limit > apiPageCap {
		return errors.New("--limit must be between 0 and 10000")
	}
	if all && after != "" {
		return errors.New("--after cannot be combined with --all")
	}
	return nil
}

func apiWithClient(cmd *cobra.Command, action func(*apiclient.Client, context.Context) error) error {
	ctx, cancel, err := apiCommandContext(cmd, 60*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	return action(client, ctx)
}

func apiClientError(err error) error {
	return &exitCodeError{code: apiclient.ExitCodeFor(err), err: err}
}

var (
	sessionColumns = []string{"id", "title", "created_at", "message_count"}
	messageColumns = []string{"id", "role", "created_at"}
	modelColumns   = []string{"id", "name", "provider"}
)

func printAPIItems[T any](cmd *cobra.Command, items []T, columns []string) error {
	if apiJSON(cmd) {
		for _, item := range items {
			if err := printAPIJSON(cmd, item); err != nil {
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
			return apiConfigurationError(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			return apiConfigurationError(err)
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

func printAPISingle(cmd *cobra.Command, item any) error {
	if apiJSON(cmd) {
		return printAPIJSON(cmd, item)
	}
	data, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return apiConfigurationError(err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return err
}

func printAPIJSON(cmd *cobra.Command, item any) error {
	data, err := json.Marshal(item)
	if err != nil {
		return apiConfigurationError(err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return err
}
