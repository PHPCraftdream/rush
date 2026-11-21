package apictl

import (
	"context"
	"net/http"

	"github.com/PHPCraftdream/rush/internal/serve/client/apiclient"
	apisessions "github.com/PHPCraftdream/rush/internal/serve/cmd/admin/serverctl/api/sessions"
	"github.com/spf13/cobra"
)

func newAPIOpenAPICmd() *cobra.Command {
	return &cobra.Command{Use: "openapi", Short: "Print the remote OpenAPI document", Long: "Write the raw OpenAPI document to stdout." + apisessions.ApiLeafHelp, Example: "rush api openapi", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return apisessions.ApiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			data, err := c.OpenAPI(ctx)
			if err != nil {
				return apisessions.ApiClientError(err)
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		})
	}}
}

func validAPIMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}
