// Fork addition: `qwen-init` installs the `rush`/`rush-fallback` slash
// commands for Qwen Code CLI (`.qwen/commands/*.md`). Part of the
// `<tool>-init`/`<tool>-del` family alongside claude-init/claude-del and
// codex-init/codex-del; gemini-init/gemini-del and grok-init/grok-del
// follow the same pattern, converting from the same canonical source
// templates (claudeSlashCommandTemplate / claudeFallbackCommandTemplate,
// embedded in claude_init.go) via the helpers in multi_cli_convert.go.
//
// Qwen Code CLI's custom-command convention is structurally almost
// identical to Claude Code's: a flat Markdown file with `description:`
// YAML front-matter, no Skills-style subdirectory. The only difference
// is the in-body argument placeholder — Qwen uses `{{args}}` instead of
// Claude's `$ARGUMENTS`.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

const (
	qwenCommandsDir     = ".qwen/commands" // relative to cwd (local) or $HOME (global)
	qwenArgsPlaceholder = "{{args}}"
)

// resolveQwenCommandsDir returns the directory Qwen Code CLI custom
// commands should be written to. When global is true it returns
// ~/.qwen/commands; otherwise <cwd>/.qwen/commands. Mirrors
// resolveCommandsDir's shape (no repo-root search upward).
func resolveQwenCommandsDir(cwd string, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, qwenCommandsDir), nil
	}
	return filepath.Join(cwd, qwenCommandsDir), nil
}

var qwenInitCmd = &cobra.Command{
	Use:   "qwen-init",
	Short: "Install the rush/rush-fallback slash-commands for Qwen Code CLI",
	Long: `Set up rush's delegation commands in Qwen Code CLI.

Qwen Code CLI custom commands are written to ` + "`~/.qwen/commands/rush.md`" + ` and
` + "`~/.qwen/commands/rush-fallback.md`" + ` by default (the GLOBAL scope,
available in every project). Use --local (or --cwd, which implies it) to
scope them to the current project's ` + "`.qwen/commands/`" + ` instead.

Content is converted from the same canonical source used by
` + "`claude-init`" + ` — the two stay in sync automatically. The only format
difference is the in-body argument placeholder: Qwen Code CLI uses
` + "`{{args}}`" + ` where Claude Code uses ` + "`$ARGUMENTS`" + `.

Skipped (with a warning) if a target file exists without our sentinel —
we never overwrite a file we don't own.`,
	Example: `
# Install / refresh the Qwen commands globally — the default
rush qwen-init

# Install into the current project instead
rush qwen-init --local

# Scope to another project (implies --local)
rush qwen-init --cwd /path/to/project
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

		return installQwenCommands(commandsDir)
	},
}

// installQwenCommands writes both the rush and rush-fallback commands
// into commandsDir. Extracted so qwen_init_test.go can drive it directly.
func installQwenCommands(commandsDir string) error {
	desc1, body1, err := parseSlashCommandSource(claudeSlashCommandTemplate)
	if err != nil {
		return fmt.Errorf("rush command: %w", err)
	}
	content1 := renderFrontMatterMD(claudeSlashCommandSentinel, desc1, body1, qwenArgsPlaceholder)
	if err := writeSentinelledFile(filepath.Join(commandsDir, "rush.md"), claudeSlashCommandSentinel, content1); err != nil {
		return fmt.Errorf("rush command: %w", err)
	}

	desc2, body2, err := parseSlashCommandSource(claudeFallbackCommandTemplate)
	if err != nil {
		return fmt.Errorf("rush-fallback command: %w", err)
	}
	content2 := renderFrontMatterMD(claudeSlashCommandSentinel, desc2, body2, qwenArgsPlaceholder)
	if err := writeSentinelledFile(filepath.Join(commandsDir, "rush-fallback.md"), claudeSlashCommandSentinel, content2); err != nil {
		return fmt.Errorf("rush-fallback command: %w", err)
	}
	return nil
}

func init() {
	qwenInitCmd.Flags().Bool("global", false, "Install into ~/.qwen/commands/ (available in every project). Default when neither --global nor --local is given.")
	qwenInitCmd.Flags().Bool("local", false, "Install into the current project's .qwen/commands/ instead of ~/.qwen/commands/.")
	rootCmd.AddCommand(qwenInitCmd)
}
