// OpenCode command remover; only files carrying Rush's sentinel are deleted.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/spf13/cobra"
)

var opencodeDelCmd = &cobra.Command{
	Use: "opencode-del", Short: "Remove Rush delegation commands from OpenCode",
	Args: cobra.NoArgs,
	Long: "Remove Rush-owned /rush, /rush-fallback, /wrush and /wcrush commands. Defaults to ~/.config/opencode/commands; --local or --cwd targets <cwd>/.opencode/commands. Foreign files are preserved.",
	RunE: runOpenCodeDel,
}

func runOpenCodeDel(cmd *cobra.Command, _ []string) error {
	dir, err := nativeCommandDir(cmd, ".config/opencode/commands", opencodeCommandsDir)
	if err != nil {
		return err
	}
	return removeMarkdownCommands(dir)
}

func nativeCommandDir(cmd *cobra.Command, globalPath, localPath string) (string, error) {
	global, _ := cmd.Flags().GetBool("global")
	local, _ := cmd.Flags().GetBool("local")
	localMode := local || cmd.Flags().Changed("cwd")
	if global && localMode {
		return "", fmt.Errorf("--global and --local/--cwd are mutually exclusive")
	}
	if localMode {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, localPath), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, filepath.FromSlash(globalPath)), nil
}

func removeMarkdownCommands(dir string) error {
	for _, name := range []string{"rush.md", "rush-fallback.md", "wrush.md", "wcrush.md"} {
		if err := removeSentinelledFile(filepath.Join(dir, name), claudeSlashCommandSentinel); err != nil {
			return err
		}
	}
	return nil
}

var newOpencodeDelCmdOnce sync.Once

func NewOpencodeDelCmd() *cobra.Command {
	newOpencodeDelCmdOnce.Do(func() {
		opencodeDelCmd.Flags().Bool("global", false, "Remove globally (default).")
		opencodeDelCmd.Flags().Bool("local", false, "Remove from the current project's .opencode/commands/.")
	})
	return deepFreshCmd(opencodeDelCmd)
}
