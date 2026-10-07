// OMP native Markdown-command installer for Rush delegation commands.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

const ompCommandsDir = ".omp/commands"

var ompInitCmd = &cobra.Command{
	Use: "omp-init", Short: "Install Rush delegation commands for Oh My Pi",
	Long: "Install /rush, /rush-fallback, /wrush and /wcrush as OMP Markdown commands. Defaults to ~/.omp/agent/commands; use --local or --cwd for <cwd>/.omp/commands. Existing foreign files are preserved.",
	RunE: runOMPInit,
}

func runOMPInit(cmd *cobra.Command, _ []string) error {
	global, _ := cmd.Flags().GetBool("global")
	local, _ := cmd.Flags().GetBool("local")
	localMode := local || cmd.Flags().Changed("cwd")
	if global && localMode {
		return fmt.Errorf("--global and --local/--cwd are mutually exclusive")
	}
	var dir string
	if localMode {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		dir = filepath.Join(cwd, ompCommandsDir)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home directory: %w", err)
		}
		dir = filepath.Join(home, ".omp", "agent", "commands")
	}
	return installMarkdownCommands(dir, skillTargetOmp)
}

func init() {
	ompInitCmd.Flags().Bool("global", false, "Install globally (default).")
	ompInitCmd.Flags().Bool("local", false, "Install into the current project's .omp/commands/.")
	rootCmd.AddCommand(ompInitCmd)
}
