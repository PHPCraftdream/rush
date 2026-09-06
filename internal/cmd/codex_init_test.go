package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/skills"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func runCodexInitInDir(t *testing.T, dir string) {
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
	require.NoError(t, cmd.ParseFlags([]string{"--cwd", dir}))
	require.NoError(t, codexInitCmd.RunE(cmd, nil))
}

func runCodexDelInDir(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, runCodexDel(dir))
}

func assertCodexSkillFrontmatter(t *testing.T, content []byte, name, canonicalTemplate string) {
	t.Helper()
	description, _, err := parseSlashCommandSource(canonicalTemplate)
	require.NoError(t, err)

	wantPrefix := "---\nname: " + name + "\ndescription: " + description + "\n---\n" +
		claudeSlashCommandSentinel + "\n\n"
	require.True(t, strings.HasPrefix(string(content), wantPrefix), "generated %s SKILL.md must begin with YAML frontmatter followed by the ownership sentinel", name)

	parsed, err := skills.ParseContent(content)
	require.NoError(t, err, "generated %s SKILL.md must parse as an Agent Skill", name)
	assert.Equal(t, name, parsed.Name)
	assert.Equal(t, description, parsed.Description)
	assert.True(t, strings.HasPrefix(parsed.Instructions, claudeSlashCommandSentinel+"\n"))
}

// ---------------------------------------------------------------------------
// codex-init tests
// ---------------------------------------------------------------------------

func TestCodexInit_CreatesSlashCommand(t *testing.T) {
	dir := t.TempDir()
	runCodexInitInDir(t, dir)

	skillPath := filepath.Join(dir, ".agents", "skills", "rush", "SKILL.md")
	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assertCodexSkillFrontmatter(t, bts, "rush", claudeSlashCommandTemplate)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "rush run")
	assert.Contains(t, got, "--role smart")
	assert.Contains(t, got, "name: rush")
}

func TestCodexInit_GlobalScopeCreatesAllDelegationSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	cmd := &cobra.Command{}
	cmd.Flags().Bool("global", false, "")
	cmd.Flags().Bool("local", false, "")
	cmd.Flags().StringP("cwd", "c", "", "")
	require.NoError(t, cmd.ParseFlags([]string{"--global"}))
	require.NoError(t, codexInitCmd.RunE(cmd, nil))

	for _, name := range []string{"rush", "rush-fallback", "wrush"} {
		_, err := os.Stat(filepath.Join(home, ".agents", "skills", name, "SKILL.md"))
		require.NoError(t, err, "global %s Skill should be installed", name)
	}
}

func TestCodexInit_CreatesFallbackSkill(t *testing.T) {
	dir := t.TempDir()
	runCodexInitInDir(t, dir)

	skillPath := filepath.Join(dir, ".agents", "skills", "rush-fallback", "SKILL.md")
	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assertCodexSkillFrontmatter(t, bts, "rush-fallback", claudeFallbackCommandTemplate)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "CronCreate")
	assert.Contains(t, got, "TaskCreate")
	assert.Contains(t, got, "name: rush-fallback")
}

func TestCodexInit_CreatesWrushSkillFromCanonicalTemplate(t *testing.T) {
	dir := t.TempDir()
	runCodexInitInDir(t, dir)

	skillPath := filepath.Join(dir, ".agents", "skills", "wrush", "SKILL.md")
	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assertCodexSkillFrontmatter(t, bts, "wrush", claudeWrushCommandTemplate)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "name: wrush")
	assert.Contains(t, got, "dedicated git worktree")
	assert.NotContains(t, got, "`rush.md` file in this same directory")
	assert.NotContains(t, got, "rush.md")
	assert.Contains(t, got, "sibling `../rush/SKILL.md` file")
}

func TestCodexInit_SlashCommandOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runCodexInitInDir(t, dir)
	skillPath := filepath.Join(dir, ".agents", "skills", "rush", "SKILL.md")
	first, err := os.ReadFile(skillPath)
	require.NoError(t, err)

	runCodexInitInDir(t, dir)
	second, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}

func TestCodexInit_WrushOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runCodexInitInDir(t, dir)
	skillPath := filepath.Join(dir, ".agents", "skills", "wrush", "SKILL.md")
	require.NoError(t, os.WriteFile(skillPath, []byte(claudeSlashCommandSentinel+"\nold content\n"), 0o644))

	runCodexInitInDir(t, dir)
	got, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assert.Contains(t, string(got), "dedicated git worktree")
	assert.NotContains(t, string(got), "old content")
}

func TestCodexInit_MigratesSentinelFirstOwnedSkills(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, ".agents", "skills")
	tests := []struct {
		name     string
		template string
	}{
		{name: "rush", template: claudeSlashCommandTemplate},
		{name: "rush-fallback", template: claudeFallbackCommandTemplate},
		{name: "wrush", template: claudeWrushCommandTemplate},
	}
	for _, test := range tests {
		path := filepath.Join(skillsDir, test.name, "SKILL.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(claudeSlashCommandSentinel+"\nlegacy content\n"), 0o644))
	}

	require.NoError(t, installCodexSkills(skillsDir))
	for _, test := range tests {
		content, err := os.ReadFile(filepath.Join(skillsDir, test.name, "SKILL.md"))
		require.NoError(t, err)
		assertCodexSkillFrontmatter(t, content, test.name, test.template)
		assert.NotContains(t, string(content), "legacy content")
	}
}

func TestWriteCodexSkillRejectsInvalidFrontmatter(t *testing.T) {
	dir := t.TempDir()
	err := writeCodexSkill(dir, "rush", claudeSlashCommandSentinel+"\ninvalid")
	require.ErrorContains(t, err, "validate generated SKILL.md")
	_, statErr := os.Stat(filepath.Join(dir, "rush", "SKILL.md"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestCodexInit_SlashCommandSkipsWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".agents", "skills", "rush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o755))
	require.NoError(t, os.WriteFile(skillPath, []byte("someone else's file"), 0o644))

	stderr := captureStderr(t, func() {
		runCodexInitInDir(t, dir)
	})

	assert.Contains(t, stderr, "does not contain our sentinel")
	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

func TestCodexInit_WrushSkipsWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".agents", "skills", "wrush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o755))
	require.NoError(t, os.WriteFile(skillPath, []byte("someone else's file"), 0o644))

	stderr := captureStderr(t, func() {
		runCodexInitInDir(t, dir)
	})

	assert.Contains(t, stderr, "does not contain our sentinel")
	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

// ---------------------------------------------------------------------------
// codex-del tests
// ---------------------------------------------------------------------------

func TestCodexDel_RemovesSlashCommandWithSentinel(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".agents", "skills", "rush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o755))
	require.NoError(t, os.WriteFile(skillPath, []byte("<!-- rush-slash-command:v1 -->\nsome content\n"), 0o644))

	runCodexDelInDir(t, dir)

	_, err := os.Stat(skillPath)
	assert.True(t, os.IsNotExist(err), "skill file should be removed when it has our sentinel")

	// The now-empty rush/ skill directory should be cleaned up too.
	_, err = os.Stat(filepath.Dir(skillPath))
	assert.True(t, os.IsNotExist(err), "now-empty skill directory should be removed")
}

func TestCodexDel_RefusesSlashCommandWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".agents", "skills", "rush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o755))
	require.NoError(t, os.WriteFile(skillPath, []byte("not ours"), 0o644))

	stderr := captureStderr(t, func() {
		runCodexDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "refusing to delete")
	assert.Contains(t, stderr, "missing sentinel")

	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assert.Equal(t, "not ours", string(bts))
}

func TestCodexDel_RemovesWrushWithSentinel(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".agents", "skills", "wrush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o755))
	require.NoError(t, os.WriteFile(skillPath, []byte(claudeSlashCommandSentinel+"\nwrush content\n"), 0o644))

	runCodexDelInDir(t, dir)

	_, err := os.Stat(skillPath)
	assert.True(t, os.IsNotExist(err), "wrush skill should be removed when it has our sentinel")
	_, err = os.Stat(filepath.Dir(skillPath))
	assert.True(t, os.IsNotExist(err), "now-empty wrush skill directory should be removed")
}

func TestCodexDel_RefusesWrushWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".agents", "skills", "wrush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o755))
	require.NoError(t, os.WriteFile(skillPath, []byte("not ours"), 0o644))

	stderr := captureStderr(t, func() {
		runCodexDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "refusing to delete")
	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assert.Equal(t, "not ours", string(bts))
}

func TestCodexDel_IdempotentOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	runCodexInitInDir(t, dir)

	runCodexDelInDir(t, dir)
	stderr := captureStderr(t, func() {
		runCodexDelInDir(t, dir)
	})
	// Second run is a no-op: nothing left to remove, no errors raised.
	assert.NotContains(t, stderr, "refusing to delete")
}

// TestCodexDel_RemovesLegacyPrerenameInstall verifies that codex-del removes
// both legacy crush/crush-fallback Skills (from pre-rename installs) and the
// new rush/rush-fallback/wrush Skills, while leaving foreign files alone.
func TestCodexDel_RemovesLegacyPrerenameInstall(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, ".agents", "skills")
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))

	// Seed legacy crush/SKILL.md with legacy sentinel
	legacyCrushPath := filepath.Join(skillsDir, "crush", "SKILL.md")
	legacyCrushContent := "<!-- crush-slash-command:v1 -->\nlegacy crush skill\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(legacyCrushPath), 0o755))
	require.NoError(t, os.WriteFile(legacyCrushPath, []byte(legacyCrushContent), 0o644))

	// Seed legacy crush-fallback/SKILL.md with legacy sentinel
	legacyFallbackPath := filepath.Join(skillsDir, "crush-fallback", "SKILL.md")
	legacyFallbackContent := "<!-- crush-slash-command:v1 -->\nlegacy fallback skill\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(legacyFallbackPath), 0o755))
	require.NoError(t, os.WriteFile(legacyFallbackPath, []byte(legacyFallbackContent), 0o644))

	// Seed rush/SKILL.md with new sentinel
	rushPath := filepath.Join(skillsDir, "rush", "SKILL.md")
	rushContent := "<!-- rush-slash-command:v1 -->\nnew rush skill\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(rushPath), 0o755))
	require.NoError(t, os.WriteFile(rushPath, []byte(rushContent), 0o644))

	// Seed rush-fallback/SKILL.md with new sentinel
	rushFallbackPath := filepath.Join(skillsDir, "rush-fallback", "SKILL.md")
	rushFallbackContent := "<!-- rush-slash-command:v1 -->\nnew rush fallback skill\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(rushFallbackPath), 0o755))
	require.NoError(t, os.WriteFile(rushFallbackPath, []byte(rushFallbackContent), 0o644))

	// Seed wrush/SKILL.md with new sentinel
	wrushPath := filepath.Join(skillsDir, "wrush", "SKILL.md")
	wrushContent := "<!-- rush-slash-command:v1 -->\nnew wrush skill\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(wrushPath), 0o755))
	require.NoError(t, os.WriteFile(wrushPath, []byte(wrushContent), 0o644))

	// Seed a foreign crush/SKILL.md WITHOUT any sentinel - should survive
	foreignCrushPath := filepath.Join(skillsDir, "foreign-crush", "SKILL.md")
	foreignContent := "This is a foreign skill without our sentinel\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(foreignCrushPath), 0o755))
	require.NoError(t, os.WriteFile(foreignCrushPath, []byte(foreignContent), 0o644))

	// Run codex-del
	stderr := captureStderr(t, func() {
		require.NoError(t, runCodexDel(dir))
	})

	// All files with either sentinel should be removed
	_, err := os.Stat(legacyCrushPath)
	assert.True(t, os.IsNotExist(err), "legacy crush/SKILL.md with legacy sentinel should be removed")

	_, err = os.Stat(legacyFallbackPath)
	assert.True(t, os.IsNotExist(err), "legacy crush-fallback/SKILL.md with legacy sentinel should be removed")

	_, err = os.Stat(rushPath)
	assert.True(t, os.IsNotExist(err), "rush/SKILL.md with new sentinel should be removed")

	_, err = os.Stat(rushFallbackPath)
	assert.True(t, os.IsNotExist(err), "rush-fallback/SKILL.md with new sentinel should be removed")

	_, err = os.Stat(wrushPath)
	assert.True(t, os.IsNotExist(err), "wrush/SKILL.md with new sentinel should be removed")

	// Foreign skill without sentinel should survive
	foreignData, err := os.ReadFile(foreignCrushPath)
	require.NoError(t, err)
	assert.Equal(t, foreignContent, string(foreignData), "foreign skill without sentinel should survive")

	// Verify stderr mentions all removals
	assert.Contains(t, stderr, "removed")
	assert.Contains(t, stderr, "crush")
	assert.Contains(t, stderr, "rush")
}
