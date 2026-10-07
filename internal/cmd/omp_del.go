// OMP command remover; only files carrying Rush's sentinel are deleted.
package cmd

import (
	"github.com/spf13/cobra"
)

var ompDelCmd = &cobra.Command{
	Use: "omp-del", Short: "Remove Rush delegation commands from Oh My Pi",
	Long: "Remove Rush-owned /rush, /rush-fallback, /wrush and /wcrush commands. Defaults to ~/.omp/agent/commands; --local or --cwd targets <cwd>/.omp/commands. Foreign files are preserved.",
	RunE: runOMPDel,
}

func runOMPDel(cmd *cobra.Command, _ []string) error {
	dir, err := nativeCommandDir(cmd, ".omp/agent/commands", ompCommandsDir)
	if err != nil {
		return err
	}
	return removeMarkdownCommands(dir)
}

func init() {
	ompDelCmd.Flags().Bool("global", false, "Remove globally (default).")
	ompDelCmd.Flags().Bool("local", false, "Remove from the current project's .omp/commands/.")
	rootCmd.AddCommand(ompDelCmd)
}
