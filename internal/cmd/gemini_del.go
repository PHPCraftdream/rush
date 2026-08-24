// Fork addition: `gemini-del` undoes `gemini-init` — removes the
// rush/rush-fallback Gemini CLI custom commands. See gemini_init.go for
// context.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var geminiDelCmd = &cobra.Command{
	Use:   "gemini-del",
	Short: "Remove the rush/rush-fallback custom commands from Gemini CLI",
	Long: `Undo ` + "`rush gemini-init`" + `: remove the rush and rush-fallback custom
commands from Gemini CLI's commands directory.

Only files that carry our sentinel are removed — foreign .toml files with
the same name are left alone with a warning.

This also removes legacy crush and crush-fallback commands from a
pre-rename install (if they contain the legacy sentinel).

Default is --global (~/.gemini/commands/). Use --local (or --cwd, which
implies it) to target the current project's .gemini/commands/ instead.
--global and --local/--cwd are mutually exclusive.

Idempotent: running this twice is a no-op the second time.`,
	Example: `
# Remove globally (from ~/.gemini/commands/) — the default
rush gemini-del

# Remove from the current project instead
rush gemini-del --local

# Scope to another project (implies --local)
rush gemini-del --cwd /path/to/project
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
			commandsDir, err = resolveGeminiCommandsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			commandsDir, err = resolveGeminiCommandsDir("", true)
			if err != nil {
				return err
			}
		}

		return removeGeminiCommands(commandsDir)
	},
}

// runGeminiDel is kept for tests that call it directly (local mode only).
func runGeminiDel(cwd string) error {
	commandsDir, err := resolveGeminiCommandsDir(cwd, false)
	if err != nil {
		return err
	}
	return removeGeminiCommands(commandsDir)
}

const (
	legacyGeminiSlashCommandSentinel = "crush-slash-command:v1"
)

// containsAnyGeminiSentinel returns true if the data contains either the new
// rush-slash-command sentinel or the legacy crush-slash-command sentinel.
func containsAnyGeminiSentinel(data string) bool {
	return strings.Contains(data, geminiSlashCommandSentinel) ||
		strings.Contains(data, legacyGeminiSlashCommandSentinel)
}

// removeGeminiCommands removes both the rush/rush-fallback commands and the legacy
// crush/crush-fallback commands from commandsDir.
func removeGeminiCommands(commandsDir string) error {
	// Remove new rush-named commands
	if err := removeSentinelledFile(filepath.Join(commandsDir, "rush.toml"), geminiSlashCommandSentinel); err != nil {
		return err
	}
	if err := removeSentinelledFile(filepath.Join(commandsDir, "rush-fallback.toml"), geminiSlashCommandSentinel); err != nil {
		return err
	}
	// Remove legacy crush-named commands (accept either sentinel)
	if err := removeGeminiFileWithEitherSentinel(commandsDir, "crush.toml"); err != nil {
		return err
	}
	return removeGeminiFileWithEitherSentinel(commandsDir, "crush-fallback.toml")
}

// removeGeminiFileWithEitherSentinel removes filepath.Join(commandsDir, name) if it
// contains either the new rush-slash-command sentinel or the legacy
// crush-slash-command sentinel.
func removeGeminiFileWithEitherSentinel(commandsDir, name string) error {
	path := filepath.Join(commandsDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !containsAnyGeminiSentinel(string(data)) {
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
	geminiDelCmd.Flags().Bool("global", false, "Remove from ~/.gemini/commands/. Default when neither --global nor --local is given.")
	geminiDelCmd.Flags().Bool("local", false, "Remove from the current project's .gemini/commands/ instead of ~/.gemini/commands/.")
	rootCmd.AddCommand(geminiDelCmd)
}
