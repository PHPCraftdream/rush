// Fork addition: `qwen-del` undoes `qwen-init` — removes the
// rush/rush-fallback Qwen Code CLI slash-commands. See qwen_init.go
// for context.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var qwenDelCmd = &cobra.Command{
	Use:   "qwen-del",
	Short: "Remove the rush/rush-fallback slash-commands from Qwen Code CLI",
	Long: `Undo ` + "`rush qwen-init`" + `: remove the rush and rush-fallback commands
from Qwen Code CLI's commands directory.

Only files that carry our sentinel are removed — foreign files with the
same name are left alone with a warning.

This also removes legacy crush and crush-fallback commands from a
pre-rename install (if they contain the legacy sentinel).

Default is --global (~/.qwen/commands/). Use --local (or --cwd, which
implies it) to target the current project's .qwen/commands/ instead.
--global and --local/--cwd are mutually exclusive.

Idempotent: running this twice is a no-op the second time.`,
	Example: `
# Remove globally (from ~/.qwen/commands/) — the default
rush qwen-del

# Remove from the current project instead
rush qwen-del --local

# Scope to another project (implies --local)
rush qwen-del --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		global, _ := cmd.Flags().GetBool("global")
		local, _ := cmd.Flags().GetBool("local")
		hasCwd := cmd.Flags().Changed("cwd")
		localMode := local || hasCwd

		if global && localMode {
			return fmt.Errorf("--global and --local/--cwd are mutually exclusive")
		}

		var commandsDir string
		if localMode {
			cwd, err := ResolveCwd(cmd)
			if err != nil {
				return err
			}
			commandsDir, err = resolveQwenCommandsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			commandsDir, err = resolveQwenCommandsDir("", true)
			if err != nil {
				return err
			}
		}

		return removeQwenCommands(commandsDir)
	},
}

// runQwenDel is kept for tests that call it directly (local mode only).
func runQwenDel(cwd string) error {
	commandsDir, err := resolveQwenCommandsDir(cwd, false)
	if err != nil {
		return err
	}
	return removeQwenCommands(commandsDir)
}

const (
	legacyQwenSlashCommandSentinel = "<!-- crush-slash-command:v1 -->"
)

// containsAnyQwenSentinel returns true if the data contains either the new
// rush-slash-command sentinel or the legacy crush-slash-command sentinel.
func containsAnyQwenSentinel(data string) bool {
	return strings.Contains(data, claudeSlashCommandSentinel) ||
		strings.Contains(data, legacyQwenSlashCommandSentinel)
}

// removeQwenCommands removes both the rush/rush-fallback commands and the legacy
// crush/crush-fallback commands from commandsDir.
func removeQwenCommands(commandsDir string) error {
	// Remove new rush-named commands
	if err := removeSentinelledFile(filepath.Join(commandsDir, "rush.md"), claudeSlashCommandSentinel); err != nil {
		return err
	}
	if err := removeSentinelledFile(filepath.Join(commandsDir, "rush-fallback.md"), claudeSlashCommandSentinel); err != nil {
		return err
	}
	// Remove legacy crush-named commands (accept either sentinel)
	if err := removeQwenFileWithEitherSentinel(commandsDir, "crush.md"); err != nil {
		return err
	}
	return removeQwenFileWithEitherSentinel(commandsDir, "crush-fallback.md")
}

// removeQwenFileWithEitherSentinel removes filepath.Join(commandsDir, name) if it
// contains either the new rush-slash-command sentinel or the legacy
// crush-slash-command sentinel.
func removeQwenFileWithEitherSentinel(commandsDir, name string) error {
	path := filepath.Join(commandsDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !containsAnyQwenSentinel(string(data)) {
		fmt.Fprintf(os.Stderr, "refusing to delete %s — does not look like ours (missing sentinel)\n", path)
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("failed to remove %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "removed %s\n", path)
	return nil
}

func init() {
	qwenDelCmd.Flags().Bool("global", false, "Remove from ~/.qwen/commands/. Default when neither --global nor --local is given.")
	qwenDelCmd.Flags().Bool("local", false, "Remove from the current project's .qwen/commands/ instead of ~/.qwen/commands/.")
	rootCmd.AddCommand(qwenDelCmd)
}
