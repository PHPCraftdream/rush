// Fork addition: `codex-init` installs the `rush`/`rush-fallback`/`wrush`/`wcrush` slash
// commands as Codex CLI Skills (`.agents/skills/<name>/SKILL.md`). First of
// `<tool>-init`/`<tool>-del` family: claude, codex, gemini, grok, qwen,
// opencode and omp; converting from embedded command sources via the
// helpers in multi_cli_convert.go.
package targets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"

	"github.com/PHPCraftdream/rush/internal/agent/prompt/skills"
	"github.com/spf13/cobra"
)

const CodexSkillsDir = ".agents/skills" // relative to cwd (local) or $HOME (global)

const codexFallbackSkillDescription = "Fallback guidance when Rush is unavailable; requires operator direction"

// ResolveCodexSkillsDir returns the directory Codex CLI Skills should be
// written to. When global is true it returns ~/.agents/skills; otherwise
// <cwd>/.agents/skills. Mirrors resolveCommandsDir's shape (no repo-root
// search upward) but targets Codex's own convention.
func ResolveCodexSkillsDir(cwd string, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, CodexSkillsDir), nil
	}
	return filepath.Join(cwd, CodexSkillsDir), nil
}

var codexInitCmd = &cobra.Command{
	Use:   "codex-init",
	Args:  cobra.NoArgs,
	Short: "Install the rush/rush-fallback/wrush/wcrush Skills for Codex CLI",
	Long: `Set up rush's delegation Skills in Codex CLI.

Codex CLI Skills are written to ` + "`~/.agents/skills/rush/SKILL.md`" + `,
` + "`~/.agents/skills/rush-fallback/SKILL.md`" + `,
` + "`~/.agents/skills/wrush/SKILL.md`" + ` and
` + "`~/.agents/skills/wcrush/SKILL.md`" + ` by default (the GLOBAL scope,
available in every project). Use --local (or --cwd, which implies it) to
scope them to the current project's ` + "`.agents/skills/`" + ` instead.

Content is converted from the same canonical source used by
` + "`claude-init`" + ` — the two stay in sync automatically.
The installed Skill is Markdown; its embedded source may be Markdown or
structured YAML with target-specific blocks.

Skipped (with a warning) if a target SKILL.md exists without our sentinel
— we never overwrite a file we don't own.`,
	Example: `
# Install / refresh the Codex Skills globally — the default
rush codex-init

# Install into the current project instead
rush codex-init --local

# Scope to another project (implies --local)
rush codex-init --cwd /path/to/project
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
			skillsDir, err = ResolveCodexSkillsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			skillsDir, err = ResolveCodexSkillsDir("", true)
			if err != nil {
				return err
			}
		}

		return InstallCodexSkills(skillsDir)
	},
}

// InstallCodexSkills writes the rush, rush-fallback, wrush and wcrush Skills
// into skillsDir. Extracted so codex_init_test.go can drive it directly.
func InstallCodexSkills(skillsDir string) error {
	desc1, body1, err := LoadSkillSource("claude_slash_command", skillTargetCodex)
	if err != nil {
		return fmt.Errorf("rush skill: %w", err)
	}
	content1 := toSkillMD("rush", desc1, body1)
	_, body2, err := LoadSkillSource("claude_crush_fallback_command", skillTargetCodex)
	if err != nil {
		return fmt.Errorf("rush-fallback skill: %w", err)
	}
	content2 := toSkillMD("rush-fallback", codexFallbackSkillDescription, body2)

	desc3, body3, err := parseSlashCommandSource(claudeWrushCommandTemplate)
	if err != nil {
		return fmt.Errorf("wrush skill: %w", err)
	}
	content3, err := toCodexWrushSkillMD(desc3, body3)
	if err != nil {
		return fmt.Errorf("wrush skill: %w", err)
	}
	desc4, body4, err := parseSlashCommandSource(claudeWcrushCommandTemplate)
	if err != nil {
		return fmt.Errorf("wcrush skill: %w", err)
	}
	content4, err := toCodexWcrushSkillMD(desc4, body4)
	if err != nil {
		return fmt.Errorf("wcrush skill: %w", err)
	}
	contents := []struct {
		name    string
		content string
	}{
		{name: "rush", content: content1},
		{name: "rush-fallback", content: content2},
		{name: "wrush", content: content3},
		{name: "wcrush", content: content4},
	}
	for _, item := range contents {
		if err := writeCodexSkill(skillsDir, item.name, item.content); err != nil {
			return fmt.Errorf("%s skill: %w", item.name, err)
		}
	}
	return nil
}

func writeCodexSkill(skillsDir, name, content string) error {
	parsed, err := skills.ParseContent([]byte(content))
	if err != nil {
		return fmt.Errorf("validate generated SKILL.md: %w", err)
	}
	if err := parsed.Validate(); err != nil {
		return fmt.Errorf("validate generated SKILL.md: %w", err)
	}
	if parsed.Name != name {
		return fmt.Errorf("validate generated SKILL.md: name %q does not match %q", parsed.Name, name)
	}
	return writeSentinelledSkillDir(skillsDir, name, ClaudeSlashCommandSentinel, content)
}

// NewCodexInitCmd returns the rush codexInitCmd command tree, registering its flags and children
// exactly once (former package init body; lane K2 of the sprint0 cmd split
// removed package init registration). The command object itself is a package
// var that only this constructor mutates.
var newCodexInitCmdOnce sync.Once

func NewCodexInitCmd() *cobra.Command {
	newCodexInitCmdOnce.Do(func() {
		codexInitCmd.Flags().Bool("global", false, "Install into ~/.agents/skills/ (available in every project). Default when neither --global nor --local is given.")
		codexInitCmd.Flags().Bool("local", false, "Install into the current project's .agents/skills/ instead of ~/.agents/skills/.")
	})
	return cmdkit.DeepFreshCmd(codexInitCmd)
}

// Fork addition: `codex-del` undoes `codex-init` — removes the
// rush/rush-fallback/wrush/wcrush Codex CLI Skills. See codex_init.go for context.

var codexDelCmd = &cobra.Command{
	Use:   "codex-del",
	Args:  cobra.NoArgs,
	Short: "Remove the rush/rush-fallback/wrush/wcrush Skills from Codex CLI",
	Long: `Undo ` + "`rush codex-init`" + `: remove the rush, rush-fallback, wrush and wcrush Skills
from Codex CLI's Skills directory.

Only Skills that carry our sentinel are removed — foreign SKILL.md files
with the same name are left alone with a warning.

This also removes legacy crush and crush-fallback Skills from a
pre-rename install (if they contain the legacy sentinel).

Default is --global (~/.agents/skills/). Use --local (or --cwd, which
implies it) to target the current project's .agents/skills/ instead.
--global and --local/--cwd are mutually exclusive.

Idempotent: running this twice is a no-op the second time.`,
	Example: `
# Remove globally (from ~/.agents/skills/) — the default
rush codex-del

# Remove from the current project instead
rush codex-del --local

# Scope to another project (implies --local)
rush codex-del --cwd /path/to/project
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
			skillsDir, err = ResolveCodexSkillsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			skillsDir, err = ResolveCodexSkillsDir("", true)
			if err != nil {
				return err
			}
		}

		return RemoveCodexSkills(skillsDir)
	},
}

// runCodexDel is kept for tests that call it directly (local mode only).
func runCodexDel(cwd string) error {
	skillsDir, err := ResolveCodexSkillsDir(cwd, false)
	if err != nil {
		return err
	}
	return RemoveCodexSkills(skillsDir)
}

const (
	legacyCodexSlashCommandSentinel = "<!-- crush-slash-command:v1 -->"
)

// containsAnyCodexSentinel returns true if the data contains either the new
// rush-slash-command sentinel or the legacy crush-slash-command sentinel.
func containsAnyCodexSentinel(data string) bool {
	return strings.Contains(data, ClaudeSlashCommandSentinel) ||
		strings.Contains(data, legacyCodexSlashCommandSentinel)
}

// RemoveCodexSkills removes the rush/rush-fallback/wrush/wcrush Skills and
// the legacy crush/crush-fallback Skills from skillsDir.
func RemoveCodexSkills(skillsDir string) error {
	// Remove new rush-named Skills
	if err := removeSentinelledSkillDir(skillsDir, "rush", ClaudeSlashCommandSentinel); err != nil {
		return err
	}
	if err := removeSentinelledSkillDir(skillsDir, "rush-fallback", ClaudeSlashCommandSentinel); err != nil {
		return err
	}
	if err := removeSentinelledSkillDir(skillsDir, "wrush", ClaudeSlashCommandSentinel); err != nil {
		return err
	}
	if err := removeSentinelledSkillDir(skillsDir, "wcrush", ClaudeSlashCommandSentinel); err != nil {
		return err
	}
	// Remove legacy crush-named Skills (accept either sentinel)
	if err := removeCodexSkillDirWithEitherSentinel(skillsDir, "crush"); err != nil {
		return err
	}
	return removeCodexSkillDirWithEitherSentinel(skillsDir, "crush-fallback")
}

// removeCodexSkillDirWithEitherSentinel removes <skillsDir>/<name>/SKILL.md
// if it contains either the new rush-slash-command sentinel or the legacy
// crush-slash-command sentinel, then attempts to remove the now-presumably-empty
// directory.
func removeCodexSkillDirWithEitherSentinel(skillsDir, name string) error {
	dir := filepath.Join(skillsDir, name)
	path := filepath.Join(dir, "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !containsAnyCodexSentinel(string(data)) {
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

// NewCodexDelCmd returns the rush codexDelCmd command tree, registering its flags and children
// exactly once (former package init body; lane K2 of the sprint0 cmd split
// removed package init registration). The command object itself is a package
// var that only this constructor mutates.
var newCodexDelCmdOnce sync.Once

func NewCodexDelCmd() *cobra.Command {
	newCodexDelCmdOnce.Do(func() {
		codexDelCmd.Flags().Bool("global", false, "Remove from ~/.agents/skills/. Default when neither --global nor --local is given.")
		codexDelCmd.Flags().Bool("local", false, "Remove from the current project's .agents/skills/ instead of ~/.agents/skills/.")
	})
	return cmdkit.DeepFreshCmd(codexDelCmd)
}
