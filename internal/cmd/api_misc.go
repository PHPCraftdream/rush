package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/PHPCraftdream/rush/internal/apiclient"
	"github.com/spf13/cobra"
)

func newAPIMessagesCmd() *cobra.Command {
	var after string
	var limit int
	var all bool
	cmd := &cobra.Command{Use: "messages SESSION", Short: "List messages in a remote session", Long: "List messages in a remote session. --all follows pages up to the 10000-item cap." + apiLeafHelp, Example: "rush api messages abc123 --limit 50", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAPIPageFlags(after, limit, all); err != nil {
			return apiConfigurationError(err)
		}
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			if all {
				items, err := c.AllMessages(ctx, args[0], apiPageCap)
				if err != nil {
					return apiClientError(err)
				}
				return printAPIItems(cmd, items, messageColumns)
			}
			page, err := c.ListMessages(ctx, args[0], after, limit)
			if err != nil {
				return apiClientError(err)
			}
			return printAPIItems(cmd, page.Items, messageColumns)
		})
	}}
	cmd.Flags().StringVar(&after, "after", "", "Return items after this cursor")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum items to return (1-10000)")
	cmd.Flags().BoolVar(&all, "all", false, "Fetch all pages, up to 10000 items")
	return cmd
}

func newAPIModelsCmd() *cobra.Command {
	return &cobra.Command{Use: "models", Short: "List remote API models", Long: "List models available from the selected remote." + apiLeafHelp, Example: "rush api models", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			items, err := c.Models(ctx)
			if err != nil {
				return apiClientError(err)
			}
			return printAPIItems(cmd, items, modelColumns)
		})
	}}
}

func newAPIOpenAPICmd() *cobra.Command {
	return &cobra.Command{Use: "openapi", Short: "Print the remote OpenAPI document", Long: "Write the raw OpenAPI document to stdout." + apiLeafHelp, Example: "rush api openapi", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			data, err := c.OpenAPI(ctx)
			if err != nil {
				return apiClientError(err)
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		})
	}}
}

func newAPIRawCmd() *cobra.Command {
	var dataFile string
	cmd := &cobra.Command{Use: "raw METHOD PATH", Short: "Make a raw API request", Long: "Send a request to a path relative to /api/v1. The response body goes to stdout and HTTP status to stderr." + apiLeafHelp, Example: "rush api raw GET /sessions", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		var body []byte
		var err error
		if dataFile == "-" {
			body, err = io.ReadAll(cmd.InOrStdin())
		} else if dataFile != "" {
			body, err = os.ReadFile(dataFile)
		}
		if err != nil {
			return apiConfigurationError(err)
		}
		method := strings.ToUpper(args[0])
		if !validAPIMethod(method) {
			return apiConfigurationError(fmt.Errorf("invalid HTTP method %q", args[0]))
		}
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			response, status, err := c.Raw(ctx, method, args[1], body)
			if err != nil {
				return apiClientError(err)
			}
			if _, err := cmd.OutOrStdout().Write(response); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "HTTP %d\n", status)
			return nil
		})
	}}
	cmd.Flags().StringVar(&dataFile, "data", "", "Request body file, or - for stdin")
	return cmd
}

func validAPIMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}
