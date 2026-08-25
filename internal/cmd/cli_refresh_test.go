package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// runCLIRefreshCmd builds a fresh *cobra.Command carrying the same flags
// cliRefreshCmd itself registers, parses args into it, and invokes
// runCLIRefresh directly. Restores the process cwd afterward since
// ResolveCwd (used in local mode with no [root] arg) calls os.Chdir.
func runCLIRefreshCmd(t *testing.T, args ...string) error {
	t.Helper()
	orig, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := os.Chdir(orig); err != nil {
			t.Logf("restore cwd: %v", err)
		}
	})

	cmd := &cobra.Command{}
	cmd.Flags().StringP("cwd", "c", "", "")
	cmd.Flags().BoolP("recursive", "r", false, "")
	cmd.Flags().Bool("global", false, "")
	cmd.Flags().Bool("dry-run", false, "")
	require.NoError(t, cmd.ParseFlags(args))
	return runCLIRefresh(cmd, cmd.Flags().Args())
}

// ---------------------------------------------------------------------------
// local mode
// ---------------------------------------------------------------------------

func TestCLIRefresh_LocalReplacesLegacyClaudeAndCodexFiles(t *testing.T) {
	dir := t.TempDir()

	// Seed a legacy (crush-named) Claude slash-command carrying the legacy
	// sentinel, the way a pre-rename claude-init would have left it.
	claudeLegacy := filepath.Join(dir, ".claude", "commands", "crush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(claudeLegacy), 0o755))
	require.NoError(t, os.WriteFile(claudeLegacy, []byte(legacyClaudeSlashCommandSentinel+"\nold claude content\n"), 0o644))

	// Seed a legacy (crush-named) Codex Skill carrying the legacy sentinel.
	codexLegacy := filepath.Join(dir, ".agents", "skills", "crush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(codexLegacy), 0o755))
	require.NoError(t, os.WriteFile(codexLegacy, []byte(legacyCodexSlashCommandSentinel+"\nold codex content\n"), 0o644))

	require.NoError(t, runCLIRefreshCmd(t, "--cwd", dir))

	// Legacy files are gone (del recognized the legacy sentinel).
	_, err := os.Stat(claudeLegacy)
	assert.True(t, os.IsNotExist(err), "legacy claude crush.md should have been removed")
	_, err = os.Stat(codexLegacy)
	assert.True(t, os.IsNotExist(err), "legacy codex crush SKILL.md should have been removed")

	// New rush-named files exist with current content.
	claudeNew := filepath.Join(dir, ".claude", "commands", "rush.md")
	bts, err := os.ReadFile(claudeNew)
	require.NoError(t, err)
	assert.Contains(t, string(bts), claudeSlashCommandSentinel)

	codexNew := filepath.Join(dir, ".agents", "skills", "rush", "SKILL.md")
	bts, err = os.ReadFile(codexNew)
	require.NoError(t, err)
	assert.Contains(t, string(bts), claudeSlashCommandSentinel)

	// The fallback commands/skills are installed too (unconditional
	// del+init for both rush and rush-fallback per tool).
	_, err = os.Stat(filepath.Join(dir, ".claude", "commands", "rush-fallback.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, ".agents", "skills", "rush-fallback", "SKILL.md"))
	require.NoError(t, err)

	// Gemini/grok/qwen also got installed (all 5 run unconditionally).
	_, err = os.Stat(filepath.Join(dir, ".gemini", "commands", "rush.toml"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, ".grok", "skills", "rush", "SKILL.md"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, ".qwen", "commands", "rush.md"))
	require.NoError(t, err)
}

// TestCLIRefresh_LocalNoLegacyNoExisting covers the case where the target
// directory qualifies (it has no gate to pass in local mode — local mode
// always targets the resolved cwd regardless of presence) but has neither
// old nor new files yet. Since there is no content-detection gate, del is a
// no-op (nothing to remove) and init still creates the current files.
func TestCLIRefresh_LocalNoLegacyNoExisting(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, runCLIRefreshCmd(t, "--cwd", dir))

	claudeNew := filepath.Join(dir, ".claude", "commands", "rush.md")
	_, err := os.Stat(claudeNew)
	require.NoError(t, err, "init should create rush.md even with no pre-existing legacy or current file")

	codexNew := filepath.Join(dir, ".agents", "skills", "rush", "SKILL.md")
	_, err = os.Stat(codexNew)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// global mode
// ---------------------------------------------------------------------------

func TestCLIRefresh_GlobalMode(t *testing.T) {
	isolateGlobalPaths(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	require.NoError(t, runCLIRefreshCmd(t, "--global"))

	claudeNew := filepath.Join(home, ".claude", "commands", "rush.md")
	_, err := os.Stat(claudeNew)
	require.NoError(t, err)

	qwenNew := filepath.Join(home, ".qwen", "commands", "rush.md")
	_, err = os.Stat(qwenNew)
	require.NoError(t, err)
}

func TestCLIRefresh_RecursiveAndGlobalMutuallyExclusive(t *testing.T) {
	err := runCLIRefreshCmd(t, "--recursive", "--global")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// ---------------------------------------------------------------------------
// recursive mode
// ---------------------------------------------------------------------------

func TestCLIRefresh_RecursiveFindsNestedProjectSkipsBare(t *testing.T) {
	root := t.TempDir()

	// A nested "project" directory that already has a .claude/commands dir
	// (simulating a prior claude-init run there) — should be refreshed.
	project := filepath.Join(root, "workspace", "my-project")
	claudeDir := filepath.Join(project, ".claude", "commands")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))

	// A sibling directory with no tool-integration directory at all —
	// should be skipped entirely (no files created there).
	bare := filepath.Join(root, "workspace", "bare-dir")
	require.NoError(t, os.MkdirAll(bare, 0o755))

	require.NoError(t, runCLIRefreshCmd(t, "--recursive", root))

	// The nested project got refreshed: rush.md now exists.
	_, err := os.Stat(filepath.Join(claudeDir, "rush.md"))
	require.NoError(t, err, "nested project with existing .claude/commands should have been refreshed")

	// The bare directory was skipped: no .claude dir was created there.
	_, err = os.Stat(filepath.Join(bare, ".claude"))
	assert.True(t, os.IsNotExist(err), "bare directory with no tool-integration dir should not have been touched")

	// The root itself has no tool-integration dir either — should also be
	// skipped.
	_, err = os.Stat(filepath.Join(root, ".claude"))
	assert.True(t, os.IsNotExist(err), "root with no tool-integration dir should not have been touched")
}

func TestCLIRefresh_RecursiveGatesOnAnyOfTheFiveDirs(t *testing.T) {
	root := t.TempDir()

	// Only a codex .agents/skills dir present — should still be gated in
	// (any one of the 5 tool dirs qualifies), and all 5 tools refreshed.
	project := filepath.Join(root, "codex-only")
	require.NoError(t, os.MkdirAll(filepath.Join(project, ".agents", "skills"), 0o755))

	require.NoError(t, runCLIRefreshCmd(t, "--recursive", root))

	_, err := os.Stat(filepath.Join(project, ".agents", "skills", "rush", "SKILL.md"))
	require.NoError(t, err)
	// Claude wasn't previously installed here, but since the directory
	// qualified via the codex gate, all 5 tools run unconditionally.
	_, err = os.Stat(filepath.Join(project, ".claude", "commands", "rush.md"))
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// dry-run
// ---------------------------------------------------------------------------

func TestCLIRefresh_DryRunMakesNoChanges(t *testing.T) {
	dir := t.TempDir()

	// Seed a legacy file to make sure dry-run doesn't touch it either.
	claudeLegacy := filepath.Join(dir, ".claude", "commands", "crush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(claudeLegacy), 0o755))
	require.NoError(t, os.WriteFile(claudeLegacy, []byte(legacyClaudeSlashCommandSentinel+"\nold content\n"), 0o644))

	require.NoError(t, runCLIRefreshCmd(t, "--cwd", dir, "--dry-run"))

	// Legacy file untouched.
	bts, err := os.ReadFile(claudeLegacy)
	require.NoError(t, err)
	assert.Contains(t, string(bts), "old content")

	// No new rush-named file created anywhere.
	_, err = os.Stat(filepath.Join(dir, ".claude", "commands", "rush.md"))
	assert.True(t, os.IsNotExist(err), "dry-run must not create files")
	_, err = os.Stat(filepath.Join(dir, ".agents"))
	assert.True(t, os.IsNotExist(err), "dry-run must not create any tool directories")
	_, err = os.Stat(filepath.Join(dir, ".gemini"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dir, ".grok"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dir, ".qwen"))
	assert.True(t, os.IsNotExist(err))
}

func TestCLIRefresh_DryRunRecursiveMakesNoChanges(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(project, ".claude", "commands"), 0o755))

	require.NoError(t, runCLIRefreshCmd(t, "--recursive", "--dry-run", root))

	_, err := os.Stat(filepath.Join(project, ".claude", "commands", "rush.md"))
	assert.True(t, os.IsNotExist(err), "dry-run recursive must not create files")
}
