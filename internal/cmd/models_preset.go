package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var modelsPresetCmd = &cobra.Command{
	Use:                "preset",
	Hidden:             true,
	Short:              "(removed — use `rush models use` / `rush models list`)",
	DisableFlagParsing: true, // print the redirect even when caller passes legacy preset args.
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(os.Stderr,
			"`rush models preset` was removed in batch 11.\n"+
				"Use `rush models list` to see atoms, then `rush models use <smart> <fast>`.")
		os.Exit(2)
	},
}

func init() {
	modelsCmd.AddCommand(modelsPresetCmd)
}
