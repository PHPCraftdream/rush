// Fork addition: `gemini-init` installs the `rush`/`rush-fallback` slash
// commands as Gemini CLI custom commands (`.gemini/commands/*.toml`). Part
// of the `<tool>-init`/`<tool>-del` family alongside claude, codex, grok,
// qwen, opencode and omp, converting from embedded sources via
// the helpers in multi_cli_convert.go.
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

const GeminiCommandsDir = ".gemini/commands" // relative to cwd (local) or $HOME (global)

// geminiSlashCommandSentinel is the substring toGeminiTOML embeds as its
// first line (`# rush-slash-command:v1`). We can't reuse the full
// ClaudeSlashCommandSentinel constant here (`<!-- rush-slash-command:v1
// -->`) verbatim as the ownership-check substring — TOML uses `#` comments,
// not HTML comments, so that exact string never appears in a generated
// .toml file. This narrower substring is what actually shows up in both,
// and is used consistently by both write (InstallGeminiCommands) and
// remove (RemoveGeminiCommands).
const geminiSlashCommandSentinel = "rush-slash-command:v1"

// ResolveGeminiCommandsDir returns the directory Gemini CLI custom commands
// should be written to. When global is true it returns ~/.gemini/commands;
// otherwise <cwd>/.gemini/commands. Unlike Codex's Skills convention, Gemini
// custom commands are flat files (rush.toml, rush-fallback.toml) directly
// inside this directory, not nested under a per-command subdirectory.
func ResolveGeminiCommandsDir(cwd string, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, GeminiCommandsDir), nil
	}
	return filepath.Join(cwd, GeminiCommandsDir), nil
}

var geminiInitCmd = &cobra.Command{
	Use:   "gemini-init",
	Args:  cobra.NoArgs,
	Short: "Install the rush/rush-fallback custom commands for Gemini CLI",
	Long: `Set up rush's delegation custom commands in Gemini CLI.

Gemini CLI custom commands are written to ` + "`~/.gemini/commands/rush.toml`" + ` and
` + "`~/.gemini/commands/rush-fallback.toml`" + ` by default (the GLOBAL scope,
available in every project). Use --local (or --cwd, which implies it) to
scope them to the current project's ` + "`.gemini/commands/`" + ` instead.

Content is converted from the same canonical source used by
` + "`claude-init`" + ` — the two stay in sync automatically.

Skipped (with a warning) if a target .toml file exists without our sentinel
— we never overwrite a file we don't own.`,
	Example: `
# Install / refresh the Gemini custom commands globally — the default
rush gemini-init

# Install into the current project instead
rush gemini-init --local

# Scope to another project (implies --local)
rush gemini-init --cwd /path/to/project
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
			commandsDir, err = ResolveGeminiCommandsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			commandsDir, err = ResolveGeminiCommandsDir("", true)
			if err != nil {
				return err
			}
		}

		return InstallGeminiCommands(commandsDir)
	},
}

// InstallGeminiCommands writes both the rush and rush-fallback custom
// commands into commandsDir. Extracted so gemini_init_test.go can drive it
// directly.
func InstallGeminiCommands(commandsDir string) error {
	desc1, body1, err := LoadSkillSource("claude_slash_command", skillTargetGemini)
	if err != nil {
		return fmt.Errorf("rush command: %w", err)
	}
	content1, err := toGeminiTOML(desc1, body1)
	if err != nil {
		return fmt.Errorf("rush command: %w", err)
	}
	if err := writeSentinelledFile(filepath.Join(commandsDir, "rush.toml"), geminiSlashCommandSentinel, content1); err != nil {
		return fmt.Errorf("rush command: %w", err)
	}

	desc2, body2, err := LoadSkillSource("claude_crush_fallback_command", skillTargetGemini)
	if err != nil {
		return fmt.Errorf("rush-fallback command: %w", err)
	}
	content2, err := toGeminiTOML(desc2, body2)
	if err != nil {
		return fmt.Errorf("rush-fallback command: %w", err)
	}
	if err := writeSentinelledFile(filepath.Join(commandsDir, "rush-fallback.toml"), geminiSlashCommandSentinel, content2); err != nil {
		return fmt.Errorf("rush-fallback command: %w", err)
	}
	return nil
}

// NewGeminiInitCmd returns the rush geminiInitCmd command tree, registering its flags and children
// exactly once (former package init body; lane K2 of the sprint0 cmd split
// removed package init registration). The command object itself is a package
// var that only this constructor mutates.
var newGeminiInitCmdOnce sync.Once

func NewGeminiInitCmd() *cobra.Command {
	newGeminiInitCmdOnce.Do(func() {
		geminiInitCmd.Flags().Bool("global", false, "Install into ~/.gemini/commands/ (available in every project). Default when neither --global nor --local is given.")
		geminiInitCmd.Flags().Bool("local", false, "Install into the current project's .gemini/commands/ instead of ~/.gemini/commands/.")
	})
	return cmdkit.DeepFreshCmd(geminiInitCmd)
}

// Fork addition: `gemini-del` undoes `gemini-init` — removes the
// rush/rush-fallback Gemini CLI custom commands. See gemini_init.go for
// context.

var geminiDelCmd = &cobra.Command{
	Use:   "gemini-del",
	Args:  cobra.NoArgs,
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
			cwd, err := cmdkit.ResolveCwd(cmd)
			if err != nil {
				return err
			}
			commandsDir, err = ResolveGeminiCommandsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			commandsDir, err = ResolveGeminiCommandsDir("", true)
			if err != nil {
				return err
			}
		}

		return RemoveGeminiCommands(commandsDir)
	},
}

// runGeminiDel is kept for tests that call it directly (local mode only).
func runGeminiDel(cwd string) error {
	commandsDir, err := ResolveGeminiCommandsDir(cwd, false)
	if err != nil {
		return err
	}
	return RemoveGeminiCommands(commandsDir)
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

// RemoveGeminiCommands removes both the rush/rush-fallback commands and the legacy
// crush/crush-fallback commands from commandsDir.
func RemoveGeminiCommands(commandsDir string) error {
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

// NewGeminiDelCmd returns the rush geminiDelCmd command tree, registering its flags and children
// exactly once (former package init body; lane K2 of the sprint0 cmd split
// removed package init registration). The command object itself is a package
// var that only this constructor mutates.
var newGeminiDelCmdOnce sync.Once

func NewGeminiDelCmd() *cobra.Command {
	newGeminiDelCmdOnce.Do(func() {
		geminiDelCmd.Flags().Bool("global", false, "Remove from ~/.gemini/commands/. Default when neither --global nor --local is given.")
		geminiDelCmd.Flags().Bool("local", false, "Remove from the current project's .gemini/commands/ instead of ~/.gemini/commands/.")
	})
	return cmdkit.DeepFreshCmd(geminiDelCmd)
}
