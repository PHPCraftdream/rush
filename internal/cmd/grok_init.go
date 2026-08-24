// Fork addition: `grok-init` installs the `rush`/`rush-fallback` slash
// commands as xAI Grok Build CLI Skills (`.grok/skills/<name>/SKILL.md`).
// Part of the `<tool>-init`/`<tool>-del` family alongside claude-init/
// claude-del, codex-init/codex-del and gemini-init/gemini-del/
// qwen-init/qwen-del, converting from the same canonical source templates
// (claudeSlashCommandTemplate / claudeFallbackCommandTemplate, embedded in
// claude_init.go) via the helpers in multi_cli_convert.go.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

const grokSkillsDir = ".grok/skills" // relative to cwd (local) or $HOME (global)

// resolveGrokSkillsDir returns the directory Grok Build CLI Skills should be
// written to. When global is true it returns ~/.grok/skills; otherwise
// <cwd>/.grok/skills. Mirrors resolveCommandsDir's shape (no repo-root
// search upward) but targets Grok's own convention.
func resolveGrokSkillsDir(cwd string, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, grokSkillsDir), nil
	}
	return filepath.Join(cwd, grokSkillsDir), nil
}

var grokInitCmd = &cobra.Command{
	Use:   "grok-init",
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
			cwd, err := ResolveCwd(cmd)
			if err != nil {
				return err
			}
			skillsDir, err = resolveGrokSkillsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			skillsDir, err = resolveGrokSkillsDir("", true)
			if err != nil {
				return err
			}
		}

		return installGrokSkills(skillsDir)
	},
}

// installGrokSkills writes both the rush and rush-fallback Skills into
// skillsDir. Extracted so grok_init_test.go can drive it directly.
func installGrokSkills(skillsDir string) error {
	desc1, body1, err := parseSlashCommandSource(claudeSlashCommandTemplate)
	if err != nil {
		return fmt.Errorf("rush skill: %w", err)
	}
	content1 := toSkillMD("rush", desc1, body1)
	if err := writeSentinelledSkillDir(skillsDir, "rush", claudeSlashCommandSentinel, content1); err != nil {
		return fmt.Errorf("rush skill: %w", err)
	}

	desc2, body2, err := parseSlashCommandSource(claudeFallbackCommandTemplate)
	if err != nil {
		return fmt.Errorf("rush-fallback skill: %w", err)
	}
	content2 := toSkillMD("rush-fallback", desc2, body2)
	if err := writeSentinelledSkillDir(skillsDir, "rush-fallback", claudeSlashCommandSentinel, content2); err != nil {
		return fmt.Errorf("rush-fallback skill: %w", err)
	}
	return nil
}

func init() {
	grokInitCmd.Flags().Bool("global", false, "Install into ~/.grok/skills/ (available in every project). Default when neither --global nor --local is given.")
	grokInitCmd.Flags().Bool("local", false, "Install into the current project's .grok/skills/ instead of ~/.grok/skills/.")
	rootCmd.AddCommand(grokInitCmd)
}
