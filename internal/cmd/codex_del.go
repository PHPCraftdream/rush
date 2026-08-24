// Fork addition: `codex-del` undoes `codex-init` — removes the
// rush/rush-fallback Codex CLI Skills. See codex_init.go for context.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var codexDelCmd = &cobra.Command{
	Use:   "codex-del",
	Short: "Remove the rush/rush-fallback Skills from Codex CLI",
	Long: `Undo ` + "`rush codex-init`" + `: remove the rush and rush-fallback Skills
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
			cwd, err := ResolveCwd(cmd)
			if err != nil {
				return err
			}
			skillsDir, err = resolveCodexSkillsDir(cwd, false)
			if err != nil {
				return err
			}
		} else {
			var err error
			skillsDir, err = resolveCodexSkillsDir("", true)
			if err != nil {
				return err
			}
		}

		return removeCodexSkills(skillsDir)
	},
}

// runCodexDel is kept for tests that call it directly (local mode only).
func runCodexDel(cwd string) error {
	skillsDir, err := resolveCodexSkillsDir(cwd, false)
	if err != nil {
		return err
	}
	return removeCodexSkills(skillsDir)
}

const (
	legacyCodexSlashCommandSentinel = "<!-- crush-slash-command:v1 -->"
)

// containsAnyCodexSentinel returns true if the data contains either the new
// rush-slash-command sentinel or the legacy crush-slash-command sentinel.
func containsAnyCodexSentinel(data string) bool {
	return strings.Contains(data, claudeSlashCommandSentinel) ||
		strings.Contains(data, legacyCodexSlashCommandSentinel)
}

// removeCodexSkills removes both the rush/rush-fallback Skills and the legacy
// crush/crush-fallback Skills from skillsDir.
func removeCodexSkills(skillsDir string) error {
	// Remove new rush-named Skills
	if err := removeSentinelledSkillDir(skillsDir, "rush", claudeSlashCommandSentinel); err != nil {
		return err
	}
	if err := removeSentinelledSkillDir(skillsDir, "rush-fallback", claudeSlashCommandSentinel); err != nil {
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

func init() {
	codexDelCmd.Flags().Bool("global", false, "Remove from ~/.agents/skills/. Default when neither --global nor --local is given.")
	codexDelCmd.Flags().Bool("local", false, "Remove from the current project's .agents/skills/ instead of ~/.agents/skills/.")
	rootCmd.AddCommand(codexDelCmd)
}
