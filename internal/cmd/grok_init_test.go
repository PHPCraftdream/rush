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

func runGrokInitInDir(t *testing.T, dir string) {
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
	require.NoError(t, grokInitCmd.RunE(cmd, nil))
}

func runGrokDelInDir(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, runGrokDel(dir))
}

// ---------------------------------------------------------------------------
// grok-init tests
// ---------------------------------------------------------------------------

func TestGrokInit_CreatesSlashCommand(t *testing.T) {
	dir := t.TempDir()
	runGrokInitInDir(t, dir)

	skillPath := filepath.Join(dir, ".grok", "skills", "rush", "SKILL.md")
	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "rush run")
	assert.Contains(t, got, "--role smart")
	assert.Contains(t, got, "name: rush")
}

func TestGrokInit_CreatesFallbackSkill(t *testing.T) {
	dir := t.TempDir()
	runGrokInitInDir(t, dir)

	skillPath := filepath.Join(dir, ".grok", "skills", "rush-fallback", "SKILL.md")
	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "CronCreate")
	assert.Contains(t, got, "TaskCreate")
	assert.Contains(t, got, "name: rush-fallback")
}

func TestGrokInit_SlashCommandOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runGrokInitInDir(t, dir)
	skillPath := filepath.Join(dir, ".grok", "skills", "rush", "SKILL.md")
	first, err := os.ReadFile(skillPath)
	require.NoError(t, err)

	runGrokInitInDir(t, dir)
	second, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}

func TestGrokInit_SlashCommandSkipsWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".grok", "skills", "rush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o755))
	require.NoError(t, os.WriteFile(skillPath, []byte("someone else's file"), 0o644))

	stderr := captureStderr(t, func() {
		runGrokInitInDir(t, dir)
	})

	assert.Contains(t, stderr, "does not contain our sentinel")
	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

// ---------------------------------------------------------------------------
// grok-del tests
// ---------------------------------------------------------------------------

func TestGrokDel_RemovesSlashCommandWithSentinel(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".grok", "skills", "rush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o755))
	require.NoError(t, os.WriteFile(skillPath, []byte("<!-- rush-slash-command:v1 -->\nsome content\n"), 0o644))

	runGrokDelInDir(t, dir)

	_, err := os.Stat(skillPath)
	assert.True(t, os.IsNotExist(err), "skill file should be removed when it has our sentinel")

	// The now-empty rush/ skill directory should be cleaned up too.
	_, err = os.Stat(filepath.Dir(skillPath))
	assert.True(t, os.IsNotExist(err), "now-empty skill directory should be removed")
}

func TestGrokDel_RefusesSlashCommandWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, ".grok", "skills", "rush", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(skillPath), 0o755))
	require.NoError(t, os.WriteFile(skillPath, []byte("not ours"), 0o644))

	stderr := captureStderr(t, func() {
		runGrokDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "refusing to delete")
	assert.Contains(t, stderr, "missing sentinel")

	bts, err := os.ReadFile(skillPath)
	require.NoError(t, err)
	assert.Equal(t, "not ours", string(bts))
}

func TestGrokDel_IdempotentOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	runGrokInitInDir(t, dir)

	runGrokDelInDir(t, dir)
	stderr := captureStderr(t, func() {
		runGrokDelInDir(t, dir)
	})
	// Second run is a no-op: nothing left to remove, no errors raised.
	assert.NotContains(t, stderr, "refusing to delete")
}

// TestGrokDel_RemovesLegacyPrerenameInstall verifies that grok-del removes
// both legacy crush/crush-fallback Skills (from pre-rename installs) and the
// new rush/rush-fallback Skills, while leaving foreign files alone.
func TestGrokDel_RemovesLegacyPrerenameInstall(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, ".grok", "skills")
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

	// Seed a foreign crush/SKILL.md WITHOUT any sentinel - should survive
	foreignCrushPath := filepath.Join(skillsDir, "foreign-crush", "SKILL.md")
	foreignContent := "This is a foreign skill without our sentinel\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(foreignCrushPath), 0o755))
	require.NoError(t, os.WriteFile(foreignCrushPath, []byte(foreignContent), 0o644))

	// Run grok-del
	stderr := captureStderr(t, func() {
		require.NoError(t, runGrokDel(dir))
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

	// Foreign skill without sentinel should survive
	foreignData, err := os.ReadFile(foreignCrushPath)
	require.NoError(t, err)
	assert.Equal(t, foreignContent, string(foreignData), "foreign skill without sentinel should survive")

	// Verify stderr mentions all removals
	assert.Contains(t, stderr, "removed")
	assert.Contains(t, stderr, "crush")
	assert.Contains(t, stderr, "rush")
}
