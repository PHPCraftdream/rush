// Fork addition: `grok-del` undoes `grok-init` — removes the
// rush/rush-fallback Grok Build CLI Skills. See grok_init.go for context.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var grokDelCmd = &cobra.Command{
	Use:   "grok-del",
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

		return removeGrokSkills(skillsDir)
	},
}

// runGrokDel is kept for tests that call it directly (local mode only).
func runGrokDel(cwd string) error {
	skillsDir, err := resolveGrokSkillsDir(cwd, false)
	if err != nil {
		return err
	}
	return removeGrokSkills(skillsDir)
}

const (
	legacyGrokSlashCommandSentinel = "<!-- crush-slash-command:v1 -->"
)

// containsAnyGrokSentinel returns true if the data contains either the new
// rush-slash-command sentinel or the legacy crush-slash-command sentinel.
func containsAnyGrokSentinel(data string) bool {
	return strings.Contains(data, claudeSlashCommandSentinel) ||
		strings.Contains(data, legacyGrokSlashCommandSentinel)
}

// removeGrokSkills removes both the rush/rush-fallback Skills and the legacy
// crush/crush-fallback Skills from skillsDir.
func removeGrokSkills(skillsDir string) error {
	// Remove new rush-named Skills
	if err := removeSentinelledSkillDir(skillsDir, "rush", claudeSlashCommandSentinel); err != nil {
		return err
	}
	if err := removeSentinelledSkillDir(skillsDir, "rush-fallback", claudeSlashCommandSentinel); err != nil {
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

func init() {
	grokDelCmd.Flags().Bool("global", false, "Remove from ~/.grok/skills/. Default when neither --global nor --local is given.")
	grokDelCmd.Flags().Bool("local", false, "Remove from the current project's .grok/skills/ instead of ~/.grok/skills/.")
	rootCmd.AddCommand(grokDelCmd)
}
