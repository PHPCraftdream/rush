// Fork addition: `qwen-init` installs the `rush`/`rush-fallback` slash
// commands for Qwen Code CLI (`.qwen/commands/*.md`). Part of the
// `<tool>-init`/`<tool>-del` family: claude, codex, gemini, grok, qwen,
// opencode and omp; converting from embedded command sources via
// the helpers in multi_cli_convert.go.
//
// Qwen Code CLI's custom-command convention is structurally almost
// identical to Claude Code's: a flat Markdown file with `description:`
// YAML front-matter, no Skills-style subdirectory. The only difference
// is the in-body argument placeholder — Qwen uses `{{args}}` instead of
// Claude's `$ARGUMENTS`.
package targets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"

	"github.com/spf13/cobra"
)

const (
	QwenCommandsDir     = ".qwen/commands" // relative to cwd (local) or $HOME (global)
	qwenArgsPlaceholder = "{{args}}"
)

// ResolveQwenCommandsDir returns the directory Qwen Code CLI custom
// commands should be written to. When global is true it returns
// ~/.qwen/commands; otherwise <cwd>/.qwen/commands. Mirrors
// resolveCommandsDir's shape (no repo-root search upward).
func ResolveQwenCommandsDir(cwd string, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, QwenCommandsDir), nil
	}
	return filepath.Join(cwd, QwenCommandsDir), nil
}

var qwenInitCmd = &cobra.Command{
	Use:   "qwen-init",
	Args:  cobra.NoArgs,
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
			cwd, err := cmdkit.ResolveCwd(cmd)
			if err != nil {
				return err
			}
			commandsDir, err = ResolveQwenCommandsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			commandsDir, err = ResolveQwenCommandsDir("", true)
			if err != nil {
				return err
			}
		}

		return InstallQwenCommands(commandsDir)
	},
}

// InstallQwenCommands writes both the rush and rush-fallback commands
// into commandsDir. Extracted so qwen_init_test.go can drive it directly.
func InstallQwenCommands(commandsDir string) error {
	desc1, body1, err := LoadSkillSource("claude_slash_command", skillTargetQwen)
	if err != nil {
		return fmt.Errorf("rush command: %w", err)
	}
	content1 := RenderFrontMatterMD(ClaudeSlashCommandSentinel, desc1, body1, qwenArgsPlaceholder)
	if err := writeSentinelledFile(filepath.Join(commandsDir, "rush.md"), ClaudeSlashCommandSentinel, content1); err != nil {
		return fmt.Errorf("rush command: %w", err)
	}

	desc2, body2, err := LoadSkillSource("claude_crush_fallback_command", skillTargetQwen)
	if err != nil {
		return fmt.Errorf("rush-fallback command: %w", err)
	}
	content2 := RenderFrontMatterMD(ClaudeSlashCommandSentinel, desc2, body2, qwenArgsPlaceholder)
	if err := writeSentinelledFile(filepath.Join(commandsDir, "rush-fallback.md"), ClaudeSlashCommandSentinel, content2); err != nil {
		return fmt.Errorf("rush-fallback command: %w", err)
	}
	return nil
}

// NewQwenInitCmd returns the rush qwenInitCmd command tree, registering its flags and children
// exactly once (former package init body; lane K2 of the sprint0 cmd split
// removed package init registration). The command object itself is a package
// var that only this constructor mutates.
var newQwenInitCmdOnce sync.Once

func NewQwenInitCmd() *cobra.Command {
	newQwenInitCmdOnce.Do(func() {
		qwenInitCmd.Flags().Bool("global", false, "Install into ~/.qwen/commands/ (available in every project). Default when neither --global nor --local is given.")
		qwenInitCmd.Flags().Bool("local", false, "Install into the current project's .qwen/commands/ instead of ~/.qwen/commands/.")
	})
	return cmdkit.DeepFreshCmd(qwenInitCmd)
}

// Fork addition: `qwen-del` undoes `qwen-init` — removes the
// rush/rush-fallback Qwen Code CLI slash-commands. See qwen_init.go
// for context.

var qwenDelCmd = &cobra.Command{
	Use:   "qwen-del",
	Args:  cobra.NoArgs,
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
			cwd, err := cmdkit.ResolveCwd(cmd)
			if err != nil {
				return err
			}
			commandsDir, err = ResolveQwenCommandsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			commandsDir, err = ResolveQwenCommandsDir("", true)
			if err != nil {
				return err
			}
		}

		return RemoveQwenCommands(commandsDir)
	},
}

// runQwenDel is kept for tests that call it directly (local mode only).
func runQwenDel(cwd string) error {
	commandsDir, err := ResolveQwenCommandsDir(cwd, false)
	if err != nil {
		return err
	}
	return RemoveQwenCommands(commandsDir)
}

const (
	legacyQwenSlashCommandSentinel = "<!-- crush-slash-command:v1 -->"
)

// containsAnyQwenSentinel returns true if the data contains either the new
// rush-slash-command sentinel or the legacy crush-slash-command sentinel.
func containsAnyQwenSentinel(data string) bool {
	return strings.Contains(data, ClaudeSlashCommandSentinel) ||
		strings.Contains(data, legacyQwenSlashCommandSentinel)
}

// RemoveQwenCommands removes both the rush/rush-fallback commands and the legacy
// crush/crush-fallback commands from commandsDir.
func RemoveQwenCommands(commandsDir string) error {
	// Remove new rush-named commands
	if err := removeSentinelledFile(filepath.Join(commandsDir, "rush.md"), ClaudeSlashCommandSentinel); err != nil {
		return err
	}
	if err := removeSentinelledFile(filepath.Join(commandsDir, "rush-fallback.md"), ClaudeSlashCommandSentinel); err != nil {
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

// NewQwenDelCmd returns the rush qwenDelCmd command tree, registering its flags and children
// exactly once (former package init body; lane K2 of the sprint0 cmd split
// removed package init registration). The command object itself is a package
// var that only this constructor mutates.
var newQwenDelCmdOnce sync.Once

func NewQwenDelCmd() *cobra.Command {
	newQwenDelCmdOnce.Do(func() {
		qwenDelCmd.Flags().Bool("global", false, "Remove from ~/.qwen/commands/. Default when neither --global nor --local is given.")
		qwenDelCmd.Flags().Bool("local", false, "Remove from the current project's .qwen/commands/ instead of ~/.qwen/commands/.")
	})
	return cmdkit.DeepFreshCmd(qwenDelCmd)
}
