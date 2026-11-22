// Fork addition: `grok-init` installs the `rush`/`rush-fallback` slash
// commands as xAI Grok Build CLI Skills (`.grok/skills/<name>/SKILL.md`).
// Part of the `<tool>-init`/`<tool>-del` family alongside claude, codex,
// gemini, qwen, opencode and omp, converting from embedded sources via
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

const GrokSkillsDir = ".grok/skills" // relative to cwd (local) or $HOME (global)

// ResolveGrokSkillsDir returns the directory Grok Build CLI Skills should be
// written to. When global is true it returns ~/.grok/skills; otherwise
// <cwd>/.grok/skills. Mirrors resolveCommandsDir's shape (no repo-root
// search upward) but targets Grok's own convention.
func ResolveGrokSkillsDir(cwd string, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, GrokSkillsDir), nil
	}
	return filepath.Join(cwd, GrokSkillsDir), nil
}

var grokInitCmd = &cobra.Command{
	Use:   "grok-init",
	Args:  cobra.NoArgs,
	Short: "Install the rush/rush-fallback Skills for Grok Build CLI",
	Long: `Set up rush's delegation Skills in xAI Grok Build CLI.

Grok Build CLI Skills are written to ` + "`~/.grok/skills/rush/SKILL.md`" + ` and
` + "`~/.grok/skills/rush-fallback/SKILL.md`" + ` by default (the GLOBAL scope,
available in every project). Use --local (or --cwd, which implies it) to
scope them to the current project's ` + "`.grok/skills/`" + ` instead.

Content is converted from the same canonical source used by
` + "`claude-init`" + ` — the two stay in sync automatically.

Skipped (with a warning) if a target SKILL.md exists without our sentinel
— we never overwrite a file we don't own.`,
	Example: `
# Install / refresh the Grok Skills globally — the default
rush grok-init

# Install into the current project instead
rush grok-init --local

# Scope to another project (implies --local)
rush grok-init --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		global, _ := cmd.Flags().GetBool("global")
		local, _ := cmd.Flags().GetBool("local")
		hasCwd := cmd.Flags().Changed("cwd")
		localMode := local || hasCwd

		if global && localMode {
			return fmt.Errorf("--global and --local/--cwd are mutually exclusive")
		}

		var skillsDir string
		if localMode {
			cwd, err := cmdkit.ResolveCwd(cmd)
			if err != nil {
				return err
			}
			skillsDir, err = ResolveGrokSkillsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			skillsDir, err = ResolveGrokSkillsDir("", true)
			if err != nil {
				return err
			}
		}

		return InstallGrokSkills(skillsDir)
	},
}

// InstallGrokSkills writes both the rush and rush-fallback Skills into
// skillsDir. Extracted so grok_init_test.go can drive it directly.
func InstallGrokSkills(skillsDir string) error {
	desc1, body1, err := LoadSkillSource("claude_slash_command", skillTargetGrok)
	if err != nil {
		return fmt.Errorf("rush skill: %w", err)
	}
	content1 := toSkillMD("rush", desc1, body1)
	if err := writeSentinelledSkillDir(skillsDir, "rush", ClaudeSlashCommandSentinel, content1); err != nil {
		return fmt.Errorf("rush skill: %w", err)
	}

	desc2, body2, err := LoadSkillSource("claude_crush_fallback_command", skillTargetGrok)
	if err != nil {
		return fmt.Errorf("rush-fallback skill: %w", err)
	}
	content2 := toSkillMD("rush-fallback", desc2, body2)
	if err := writeSentinelledSkillDir(skillsDir, "rush-fallback", ClaudeSlashCommandSentinel, content2); err != nil {
		return fmt.Errorf("rush-fallback skill: %w", err)
	}
	return nil
}

// NewGrokInitCmd returns the rush grokInitCmd command tree, registering its flags and children
// exactly once (former package init body; lane K2 of the sprint0 cmd split
// removed package init registration). The command object itself is a package
// var that only this constructor mutates.
var newGrokInitCmdOnce sync.Once

func NewGrokInitCmd() *cobra.Command {
	newGrokInitCmdOnce.Do(func() {
		grokInitCmd.Flags().Bool("global", false, "Install into ~/.grok/skills/ (available in every project). Default when neither --global nor --local is given.")
		grokInitCmd.Flags().Bool("local", false, "Install into the current project's .grok/skills/ instead of ~/.grok/skills/.")
	})
	return cmdkit.DeepFreshCmd(grokInitCmd)
}

// Fork addition: `grok-del` undoes `grok-init` — removes the
// rush/rush-fallback Grok Build CLI Skills. See grok_init.go for context.

var grokDelCmd = &cobra.Command{
	Use:   "grok-del",
	Args:  cobra.NoArgs,
	Short: "Remove the rush/rush-fallback Skills from Grok Build CLI",
	Long: `Undo ` + "`rush grok-init`" + `: remove the rush and rush-fallback Skills
from Grok Build CLI's Skills directory.

Only Skills that carry our sentinel are removed — foreign SKILL.md files
with the same name are left alone with a warning.

This also removes legacy crush and crush-fallback Skills from a
pre-rename install (if they contain the legacy sentinel).

Default is --global (~/.grok/skills/). Use --local (or --cwd, which
implies it) to target the current project's .grok/skills/ instead.
--global and --local/--cwd are mutually exclusive.

Idempotent: running this twice is a no-op the second time.`,
	Example: `
# Remove globally (from ~/.grok/skills/) — the default
rush grok-del

# Remove from the current project instead
rush grok-del --local

# Scope to another project (implies --local)
rush grok-del --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		global, _ := cmd.Flags().GetBool("global")
		local, _ := cmd.Flags().GetBool("local")
		hasCwd := cmd.Flags().Changed("cwd")
		localMode := local || hasCwd

		if global && localMode {
			return fmt.Errorf("--global and --local/--cwd are mutually exclusive")
		}

		var skillsDir string
		if localMode {
			cwd, err := cmdkit.ResolveCwd(cmd)
			if err != nil {
				return err
			}
			skillsDir, err = ResolveGrokSkillsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			skillsDir, err = ResolveGrokSkillsDir("", true)
			if err != nil {
				return err
			}
		}

		return RemoveGrokSkills(skillsDir)
	},
}

// runGrokDel is kept for tests that call it directly (local mode only).
func runGrokDel(cwd string) error {
	skillsDir, err := ResolveGrokSkillsDir(cwd, false)
	if err != nil {
		return err
	}
	return RemoveGrokSkills(skillsDir)
}

const (
	legacyGrokSlashCommandSentinel = "<!-- crush-slash-command:v1 -->"
)

// containsAnyGrokSentinel returns true if the data contains either the new
// rush-slash-command sentinel or the legacy crush-slash-command sentinel.
func containsAnyGrokSentinel(data string) bool {
	return strings.Contains(data, ClaudeSlashCommandSentinel) ||
		strings.Contains(data, legacyGrokSlashCommandSentinel)
}

// RemoveGrokSkills removes both the rush/rush-fallback Skills and the legacy
// crush/crush-fallback Skills from skillsDir.
func RemoveGrokSkills(skillsDir string) error {
	// Remove new rush-named Skills
	if err := removeSentinelledSkillDir(skillsDir, "rush", ClaudeSlashCommandSentinel); err != nil {
		return err
	}
	if err := removeSentinelledSkillDir(skillsDir, "rush-fallback", ClaudeSlashCommandSentinel); err != nil {
		return err
	}
	// Remove legacy crush-named Skills (accept either sentinel)
	if err := removeGrokSkillDirWithEitherSentinel(skillsDir, "crush"); err != nil {
		return err
	}
	return removeGrokSkillDirWithEitherSentinel(skillsDir, "crush-fallback")
}

// removeGrokSkillDirWithEitherSentinel removes <skillsDir>/<name>/SKILL.md
// if it contains either the new rush-slash-command sentinel or the legacy
// crush-slash-command sentinel, then attempts to remove the now-presumably-empty
// directory.
func removeGrokSkillDirWithEitherSentinel(skillsDir, name string) error {
	dir := filepath.Join(skillsDir, name)
	path := filepath.Join(dir, "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !containsAnyGrokSentinel(string(data)) {
		fmt.Fprintf(os.Stderr, "refusing to delete %s — does not look like ours (missing sentinel)\n", path)
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("failed to remove %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "removed %s\n", path)
	_ = os.Remove(dir)
	return nil
}

// NewGrokDelCmd returns the rush grokDelCmd command tree, registering its flags and children
// exactly once (former package init body; lane K2 of the sprint0 cmd split
// removed package init registration). The command object itself is a package
// var that only this constructor mutates.
var newGrokDelCmdOnce sync.Once

func NewGrokDelCmd() *cobra.Command {
	newGrokDelCmdOnce.Do(func() {
		grokDelCmd.Flags().Bool("global", false, "Remove from ~/.grok/skills/. Default when neither --global nor --local is given.")
		grokDelCmd.Flags().Bool("local", false, "Remove from the current project's .grok/skills/ instead of ~/.grok/skills/.")
	})
	return cmdkit.DeepFreshCmd(grokDelCmd)
}
