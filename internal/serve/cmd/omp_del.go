// OMP command remover; only files carrying Rush's sentinel are deleted.
package cmd

import (
	"sync"
	"github.com/spf13/cobra"
)

var ompDelCmd = &cobra.Command{
	Use: "omp-del", Short: "Remove Rush delegation commands from Oh My Pi",
	Args: cobra.NoArgs,
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

// NewOmpDelCmd returns the rush ompDelCmd command tree, registering its flags and children
// exactly once (former package init body; lane K2 of the sprint0 cmd split
// removed package init registration). The command object itself is a package
// var that only this constructor mutates.
var newOmpDelCmdOnce sync.Once

func NewOmpDelCmd() *cobra.Command {
	newOmpDelCmdOnce.Do(func() {
	ompDelCmd.Flags().Bool("global", false, "Remove globally (default).")
	ompDelCmd.Flags().Bool("local", false, "Remove from the current project's .omp/commands/.")
	})
	return ompDelCmd
}
