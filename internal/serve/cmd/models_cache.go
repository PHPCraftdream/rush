package cmd

import (
	"fmt"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

var modelsCacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage seven-day provider model catalog caches",
	Long: `Rush caches authenticated Codex, StepFun, and Z.AI model catalogs globally for seven days.
Z.AI's public reasoning documentation has a separate seven-day cache. No API keys or OAuth tokens are stored.`,
}

var modelsCacheClearCmd = &cobra.Command{
	Use:   "clear [openai-codex|stepfun|zai|all]",
	Short: "Clear cached provider model catalogs (default: all)",
	Long: `Delete one or all seven-day provider model catalogs from the global Rush cache.
Clearing zai also clears its documented effort levels. The next Rush process fetches fresh data; restart a running WebUI server to reload its in-memory models.`,
	Example: `rush models cache clear
rush models cache clear stepfun
rush models cache clear zai`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		provider := "all"
		if len(args) > 0 {
			provider = args[0]
		}
		if err := config.ClearModelCatalogCache(provider); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Cleared %s model catalog cache. Restart the WebUI server to reload models.\n", provider)
		return nil
	},
}

func init() {
	modelsCacheCmd.AddCommand(modelsCacheClearCmd)
	modelsCmd.AddCommand(modelsCacheCmd)
}
