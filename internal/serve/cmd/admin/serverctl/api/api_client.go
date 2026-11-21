package apictl

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/PHPCraftdream/rush/internal/serve/client/apiclient"
	apisessions "github.com/PHPCraftdream/rush/internal/serve/cmd/admin/serverctl/api/sessions"
	"github.com/spf13/cobra"
)

func newAPIRawCmd() *cobra.Command {
	var dataFile string
	cmd := &cobra.Command{Use: "raw METHOD PATH", Short: "Make a raw API request", Long: "Send a request to a path relative to /api/v1. The response body goes to stdout and HTTP status to stderr." + apisessions.ApiLeafHelp, Example: "rush api raw GET /sessions", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		var body []byte
		var err error
		if dataFile == "-" {
			body, err = io.ReadAll(cmd.InOrStdin())
		} else if dataFile != "" {
			body, err = os.ReadFile(dataFile)
		}
		if err != nil {
			return apisessions.ApiConfigurationError(err)
		}
		method := strings.ToUpper(args[0])
		if !validAPIMethod(method) {
			return apisessions.ApiConfigurationError(fmt.Errorf("invalid HTTP method %q", args[0]))
		}
		return apisessions.ApiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			response, status, err := c.Raw(ctx, method, args[1], body)
			if err != nil {
				return apisessions.ApiClientError(err)
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
