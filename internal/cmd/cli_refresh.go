// Fork addition: `cli-refresh` refreshes the crush-branded slash-commands/
// Skills installed by any of the 5 `<tool>-init`/`<tool>-del` pairs
// (claude, codex, gemini, grok, qwen — see claude_init.go/claude_del.go,
// codex_init.go/codex_del.go, gemini_init.go/gemini_del.go,
// grok_init.go/grok_del.go, qwen_init.go/qwen_del.go) to their current
// rush-branded equivalents, in one shot.
//
// It is a thin orchestrator: for every targeted directory it runs each
// tool's existing del-then-init pair back to back, unconditionally. The
// per-tool sentinel checks already baked into every del/init function are
// the only safety net (own-file-safety: del only ever removes a file
// carrying that tool's sentinel; init skips with a warning if a target file
// exists without the sentinel). This command does not add a second
// detection layer on top of that.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/spf13/cobra"
)

// cliRefreshTool describes one of the 5 tool integrations refreshed by this
// command: its del/init pair (both already scope-explicit — local callers
// pass the resolved local dir, global callers pass the resolved global dir)
// and the directory whose presence gates recursive-mode targeting.
type cliRefreshTool struct {
	name string
	// resolveDir mirrors resolve<Tool>CommandsDir/resolve<Tool>SkillsDir's
	// signature: (cwd, global) -> directory.
	resolveDir func(cwd string, global bool) (string, error)
	del        func(dir string) error
	init       func(dir string) error
	// presenceDir is the directory (relative to a candidate project root)
	// whose existence gates whether recursive mode acts on that root at
	// all — a structural fact (os.Stat), not file-content/sentinel
	// inspection. E.g. ".claude/commands" for Claude.
	presenceDir string
}

// cliRefreshTools is the single source of truth for the 5 tool integrations
// this command refreshes. Built from the same resolve*Dir / del / init
// functions the standalone <tool>-init/<tool>-del commands already use, so
// there is no risk of drift between "what claude-init does" and "what
// cli-refresh does for Claude".
func cliRefreshTools() []cliRefreshTool {
	return []cliRefreshTool{
		{
			name:        "claude",
			resolveDir:  resolveCommandsDir,
			del:         removeSlashCommandAndFallback,
			init:        installClaudeCommands,
			presenceDir: claudeCommandsDir,
		},
		{
			name:        "codex",
			resolveDir:  resolveCodexSkillsDir,
			del:         removeCodexSkills,
			init:        installCodexSkills,
			presenceDir: codexSkillsDir,
		},
		{
			name:        "gemini",
			resolveDir:  resolveGeminiCommandsDir,
			del:         removeGeminiCommands,
			init:        installGeminiCommands,
			presenceDir: geminiCommandsDir,
		},
		{
			name:        "grok",
			resolveDir:  resolveGrokSkillsDir,
			del:         removeGrokSkills,
			init:        installGrokSkills,
			presenceDir: grokSkillsDir,
		},
		{
			name:        "qwen",
			resolveDir:  resolveQwenCommandsDir,
			del:         removeQwenCommands,
			init:        installQwenCommands,
			presenceDir: qwenCommandsDir,
		},
	}
}

// removeSlashCommandAndFallback bundles claude-del's two dir-scoped removal
// calls (removeSlashCommandFromDir + removeFallbackCommandFromDir) behind
// the single del(dir) shape the other 4 tools already have, so
// cliRefreshTools can treat all 5 uniformly.
func removeSlashCommandAndFallback(dir string) error {
	if err := removeSlashCommandFromDir(dir); err != nil {
		return err
	}
	return removeFallbackCommandFromDir(dir)
}

// installClaudeCommands bundles claude-init's two dir-scoped install calls
// (writeSlashCommandToDir + writeFallbackCommandToDir) behind the single
// init(dir) shape the other 4 tools already have. Deliberately does NOT
// call stripLegacyBlockFromCLAUDEMd — that is claude-init/claude-del's own
// CLAUDE.md-specific behavior, out of scope for a slash-command/Skill
// refresh.
func installClaudeCommands(dir string) error {
	if err := writeSlashCommandToDir(dir); err != nil {
		return fmt.Errorf("slash command: %w", err)
	}
	return writeFallbackCommandToDir(dir)
}

var cliRefreshCmd = &cobra.Command{
	Use:   "cli-refresh [root]",
	Short: "Refresh all 5 tool integrations' rush slash-commands/Skills (claude/codex/gemini/grok/qwen)",
	Long: `Refresh the rush-branded slash-commands/Skills installed by claude-init,
codex-init, gemini-init, grok-init and qwen-init, by running each tool's
del-then-init pair back to back.

This is useful after a rush upgrade changes the canonical slash-command
content: re-running del+init picks up the new content without you having
to invoke all 10 underlying commands by hand.

Three mutually exclusive modes:

  1. Local (default, no flag): operate on the resolved cwd (--cwd, if set,
     same as every other command in this package).
  2. --recursive/-r [root]: walk the directory tree from root (default: cwd).
     Only directories that already contain at least one tool-integration
     directory (.claude/commands, .agents/skills, .gemini/commands,
     .grok/skills, .qwen/commands) are refreshed — this avoids scattering
     fresh install files into every directory of a large tree. This is a
     directory-PRESENCE gate (os.Stat), not a content/sentinel check; the
     usual per-tool sentinel safety inside del/init is what actually decides
     whether any given file gets touched.
  3. --global: operate on each tool's global scope (~/.claude/commands,
     ~/.agents/skills, ~/.gemini/commands, ~/.grok/skills, ~/.qwen/commands).

--recursive and --global are mutually exclusive.

For every targeted directory, all 5 tools' del-then-init pair runs
unconditionally — there is no "only if a legacy file is present" gate.
The existing per-tool sentinel checks (del only removes a file carrying
that tool's sentinel; init skips with a warning if a target file exists
without the sentinel) are the only safety net, by design.

A --dry-run reports what would happen without making any changes. Since
the underlying del/init functions have no native dry-run mode, --dry-run
skips calling them entirely and instead reports which targeted
directories were found (local/global: the resolved dir; recursive: every
directory that matched the presence gate).`,
	Example: `
# Refresh the local project's installed integrations
rush cli-refresh

# Refresh every project under a directory tree that already has at least
# one tool integration installed
rush cli-refresh --recursive /path/to/workspace

# Refresh all 5 tools' global scope
rush cli-refresh --global

# Preview a recursive refresh without changing anything
rush cli-refresh --recursive --dry-run /path/to/workspace
`,
	Args: cobra.MaximumNArgs(1),
	RunE: runCLIRefresh,
}

func init() {
	cliRefreshCmd.Flags().BoolP("recursive", "r", false, "Recursively walk the directory tree, refreshing only directories that already have a tool-integration directory present")
	cliRefreshCmd.Flags().Bool("global", false, "Refresh each tool's global scope instead of a project directory")
	cliRefreshCmd.Flags().Bool("dry-run", false, "Report what would happen without making any changes")
	rootCmd.AddCommand(cliRefreshCmd)
}

func runCLIRefresh(cmd *cobra.Command, args []string) error {
	recursive, _ := cmd.Flags().GetBool("recursive")
	global, _ := cmd.Flags().GetBool("global")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	if recursive && global {
		return fmt.Errorf("--recursive and --global are mutually exclusive")
	}

	tools := cliRefreshTools()

	if global {
		for _, tool := range tools {
			dir, err := tool.resolveDir("", true)
			if err != nil {
				return fmt.Errorf("%s: %w", tool.name, err)
			}
			refreshOneDir(cmd, tool, dir, "global", dryRun)
		}
		return nil
	}

	// Determine the root path (shared with migrate's [root] arg handling).
	var root string
	var err error
	if len(args) > 0 {
		root = args[0]
		if !filepath.IsAbs(root) {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get current working directory: %w", err)
			}
			root = filepath.Join(cwd, root)
		}
	} else {
		root, err = ResolveCwd(cmd)
		if err != nil {
			return fmt.Errorf("failed to resolve working directory: %w", err)
		}
	}
	root = filepath.Clean(root)

	if !recursive {
		for _, tool := range tools {
			dir, err := tool.resolveDir(root, false)
			if err != nil {
				return fmt.Errorf("%s: %w", tool.name, err)
			}
			refreshOneDir(cmd, tool, dir, "local", dryRun)
		}
		return nil
	}

	// Recursive mode: walk the tree, refreshing only directories that
	// already contain at least one tool-integration directory.
	matched := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			cmd.Printf("walk error: %v\n", err)
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && fsext.ShouldExcludeFile(root, path) {
			return filepath.SkipDir
		}

		if !dirHasAnyToolIntegration(path, tools) {
			return nil
		}
		matched++
		for _, tool := range tools {
			dir, err := tool.resolveDir(path, false)
			if err != nil {
				cmd.Printf("%s: %v\n", tool.name, err)
				continue
			}
			refreshOneDir(cmd, tool, dir, "recursive:"+path, dryRun)
		}
		return nil
	})
	if err != nil {
		cmd.Printf("walk completed with error: %v\n", err)
	}
	if matched == 0 {
		cmd.Printf("no directories with an existing tool-integration directory found under %s\n", root)
	}
	return nil
}

// dirHasAnyToolIntegration reports whether dir already contains at least
// one of the 5 tools' integration directories (.claude/commands,
// .agents/skills, .gemini/commands, .grok/skills, .qwen/commands). This is
// a directory-PRESENCE check (os.Stat) only — it does not inspect file
// content or sentinels; that is del/init's job once a directory is
// targeted.
func dirHasAnyToolIntegration(dir string, tools []cliRefreshTool) bool {
	for _, tool := range tools {
		info, err := os.Stat(filepath.Join(dir, tool.presenceDir))
		if err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// refreshOneDir runs one tool's del-then-init pair against dir and prints a
// status line, unless dryRun is set, in which case no del/init call is made
// and only a "would refresh" line is printed (see runCLIRefresh's dry-run
// note: the underlying functions have no native dry-run mode).
func refreshOneDir(cmd *cobra.Command, tool cliRefreshTool, dir, scope string, dryRun bool) {
	if dryRun {
		cmd.Printf("[%s] would refresh %s (%s)\n", tool.name, dir, scope)
		return
	}
	if err := tool.del(dir); err != nil {
		cmd.Printf("[%s] del failed for %s (%s): %v\n", tool.name, dir, scope, err)
		return
	}
	if err := tool.init(dir); err != nil {
		cmd.Printf("[%s] init failed for %s (%s): %v\n", tool.name, dir, scope, err)
		return
	}
	cmd.Printf("[%s] refreshed %s (%s)\n", tool.name, dir, scope)
}
