// Skills-directory discovery: global defaults plus the project-level
// (working directory and git root) skill locations.
//
// Two consumers share these lists. Command loading
// (commands.LoadSkillCommands/LoadProjectSkillCommands) reads the full
// lists, so skills installed by other agent tools stay available as slash
// commands. The agent system prompt reads the *PromptSkillsDirs variants
// below, which exclude other tools' directories.
package config

import (
	"cmp"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/PHPCraftdream/rush/internal/home"
)

// GlobalSkillsDirs returns the default directories for Agent Skills.
// Skills in these directories are auto-discovered and their files can be read
// without permission prompts.
//
// This is the command-facing full list: it still includes other tools'
// directories (~/.claude/skills), which are loadable as slash commands but
// are not advertised in the agent system prompt. Use
// GlobalPromptSkillsDirs for the prompt-facing subset.
func GlobalSkillsDirs() []string {
	return globalSkillsDirs(home.Dir())
}

// globalSkillsDirs returns the default global skills directories resolved
// against homeDir, so tests can inject a home without touching home.Dir().
func globalSkillsDirs(homeDir string) []string {
	if rushSkills := os.Getenv("RUSH_SKILLS_DIR"); rushSkills != "" {
		return []string{rushSkills}
	}

	paths := []string{
		filepath.Join(home.Config(), appName, "skills"),
		filepath.Join(home.Config(), "agents", "skills"),
		// Per the Agent Skills spec, scan ~/.agents/skills
		filepath.Join(homeDir, ".agents", "skills"),
		filepath.Join(homeDir, ".claude", "skills"),
	}

	// On Windows, also load from app data on top of `$HOME/.config/rush`.
	// This is here mostly for backwards compatibility.
	if runtime.GOOS == "windows" {
		appData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		paths = append(
			paths,
			filepath.Join(appData, appName, "skills"),
			filepath.Join(appData, "agents", "skills"),
		)
	}

	return paths
}

// projectSkillSubdirs lists the conventional subdirectories where
// project-level skills are discovered. Shared across working-dir and
// git-root lookups to prevent drift when a new convention is added.
var projectSkillSubdirs = []string{
	".agents/skills",
	".rush/skills",
	".claude/skills",
	".cursor/skills",
}

// otherToolSkillSubdirs lists the directory suffixes owned by other agent
// tools. Default prompt-skill discovery skips them, command discovery
// still loads from them.
var otherToolSkillSubdirs = []string{filepath.Join(".claude", "skills"), filepath.Join(".cursor", "skills")}

// ProjectSkillsDir returns the default project directories for which Rush
// will look for skills. In addition to the working directory, it also
// checks the git working tree root so that monorepo-level skills are
// discovered when the user is inside a subdirectory.
// Working-directory paths come first so local skills take precedence
// over monorepo-level ones.
//
// This is the command-facing full list; use ProjectPromptSkillsDirs for
// the prompt-facing subset.
func ProjectSkillsDir(workingDir string) []string {
	dirs := make([]string, 0, len(projectSkillSubdirs)*2)
	for _, sub := range projectSkillSubdirs {
		dirs = append(dirs, filepath.Join(workingDir, sub))
	}

	// When the working directory is inside a git repository, also look at
	// the repository root so monorepo-level .agents/skills are found.
	//
	// Compare canonical paths rather than raw strings: git rev-parse
	// --show-toplevel returns a symlink-resolved path (e.g. macOS reports
	// /private/var/... for a working dir passed in as /var/...), so a plain
	// string comparison would treat the repo root as distinct from the
	// working dir even when they are the same directory, duplicating every
	// entry. Fall back to the raw path if EvalSymlinks fails (e.g. the path
	// does not exist).
	if root := worktreeRoot(workingDir); root != "" && !sameDir(root, workingDir) {
		for _, sub := range projectSkillSubdirs {
			dirs = append(dirs, filepath.Join(root, sub))
		}
	}

	return dirs
}

// GlobalPromptSkillsDirs returns the skill directories advertised in the
// agent system prompt: the global discovery list minus other tools'
// directories. An explicit RUSH_SKILLS_DIR override wins and is returned
// as-is.
func GlobalPromptSkillsDirs() []string {
	if os.Getenv("RUSH_SKILLS_DIR") != "" {
		return globalSkillsDirs(home.Dir())
	}
	return excludeOtherToolSkillDirs(globalSkillsDirs(home.Dir()))
}

// ProjectPromptSkillsDirs is ProjectSkillsDir minus other tools'
// directories; used for the agent system prompt only.
func ProjectPromptSkillsDirs(workingDir string) []string {
	return excludeOtherToolSkillDirs(ProjectSkillsDir(workingDir))
}

func excludeOtherToolSkillDirs(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		clean := filepath.Clean(dir)
		skip := false
		for _, sub := range otherToolSkillSubdirs {
			if strings.HasSuffix(clean, string(filepath.Separator)+sub) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, dir)
		}
	}
	return out
}
