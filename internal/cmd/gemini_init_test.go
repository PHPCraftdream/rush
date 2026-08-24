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

func runGeminiInitInDir(t *testing.T, dir string) {
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
	require.NoError(t, geminiInitCmd.RunE(cmd, nil))
}

func runGeminiDelInDir(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, runGeminiDel(dir))
}

// ---------------------------------------------------------------------------
// gemini-init tests
// ---------------------------------------------------------------------------

func TestGeminiInit_CreatesSlashCommand(t *testing.T) {
	dir := t.TempDir()
	runGeminiInitInDir(t, dir)

	commandPath := filepath.Join(dir, ".gemini", "commands", "rush.toml")
	bts, err := os.ReadFile(commandPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, "rush-slash-command:v1")
	assert.Contains(t, got, `prompt = """`)
	assert.Contains(t, got, `description = "`)
	assert.NotContains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "{{args}}")
	assert.Contains(t, got, "rush run")
	assert.Contains(t, got, "--role smart")
}

func TestGeminiInit_CreatesFallbackCommand(t *testing.T) {
	dir := t.TempDir()
	runGeminiInitInDir(t, dir)

	commandPath := filepath.Join(dir, ".gemini", "commands", "rush-fallback.toml")
	bts, err := os.ReadFile(commandPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, "rush-slash-command:v1")
	assert.Contains(t, got, `prompt = """`)
	assert.Contains(t, got, `description = "`)
	assert.NotContains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "{{args}}")
	assert.Contains(t, got, "CronCreate")
	assert.Contains(t, got, "TaskCreate")
}

func TestGeminiInit_SlashCommandOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runGeminiInitInDir(t, dir)
	commandPath := filepath.Join(dir, ".gemini", "commands", "rush.toml")
	first, err := os.ReadFile(commandPath)
	require.NoError(t, err)

	runGeminiInitInDir(t, dir)
	second, err := os.ReadFile(commandPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}

func TestGeminiInit_SlashCommandSkipsWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, ".gemini", "commands", "rush.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(commandPath), 0o755))
	require.NoError(t, os.WriteFile(commandPath, []byte("someone else's file"), 0o644))

	stderr := captureStderr(t, func() {
		runGeminiInitInDir(t, dir)
	})

	assert.Contains(t, stderr, "does not contain our sentinel")
	bts, err := os.ReadFile(commandPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

// ---------------------------------------------------------------------------
// gemini-del tests
// ---------------------------------------------------------------------------

func TestGeminiDel_RemovesSlashCommandWithSentinel(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, ".gemini", "commands", "rush.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(commandPath), 0o755))
	require.NoError(t, os.WriteFile(commandPath, []byte("# rush-slash-command:v1\nsome content\n"), 0o644))

	runGeminiDelInDir(t, dir)

	_, err := os.Stat(commandPath)
	assert.True(t, os.IsNotExist(err), "command file should be removed when it has our sentinel")
}

func TestGeminiDel_RefusesSlashCommandWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, ".gemini", "commands", "rush.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(commandPath), 0o755))
	require.NoError(t, os.WriteFile(commandPath, []byte("not ours"), 0o644))

	stderr := captureStderr(t, func() {
		runGeminiDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "refusing to delete")
	assert.Contains(t, stderr, "missing sentinel")

	bts, err := os.ReadFile(commandPath)
	require.NoError(t, err)
	assert.Equal(t, "not ours", string(bts))
}

func TestGeminiDel_IdempotentOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	runGeminiInitInDir(t, dir)

	runGeminiDelInDir(t, dir)
	stderr := captureStderr(t, func() {
		runGeminiDelInDir(t, dir)
	})
	// Second run is a no-op: nothing left to remove, no errors raised.
	assert.NotContains(t, stderr, "refusing to delete")
}

// TestGeminiDel_RemovesLegacyPrerenameInstall verifies that gemini-del removes
// both legacy crush/crush-fallback commands (from pre-rename installs) and the
// new rush/rush-fallback commands, while leaving foreign files alone.
func TestGeminiDel_RemovesLegacyPrerenameInstall(t *testing.T) {
	dir := t.TempDir()
	commandsDir := filepath.Join(dir, ".gemini", "commands")
	require.NoError(t, os.MkdirAll(commandsDir, 0o755))

	// Seed legacy crush.toml with legacy sentinel
	legacyCrushPath := filepath.Join(commandsDir, "crush.toml")
	legacyCrushContent := "# crush-slash-command:v1\nlegacy crush command\n"
	require.NoError(t, os.WriteFile(legacyCrushPath, []byte(legacyCrushContent), 0o644))

	// Seed legacy crush-fallback.toml with legacy sentinel
	legacyFallbackPath := filepath.Join(commandsDir, "crush-fallback.toml")
	legacyFallbackContent := "# crush-slash-command:v1\nlegacy fallback command\n"
	require.NoError(t, os.WriteFile(legacyFallbackPath, []byte(legacyFallbackContent), 0o644))

	// Seed rush.toml with new sentinel
	rushPath := filepath.Join(commandsDir, "rush.toml")
	rushContent := "# rush-slash-command:v1\nnew rush command\n"
	require.NoError(t, os.WriteFile(rushPath, []byte(rushContent), 0o644))

	// Seed rush-fallback.toml with new sentinel
	rushFallbackPath := filepath.Join(commandsDir, "rush-fallback.toml")
	rushFallbackContent := "# rush-slash-command:v1\nnew rush fallback command\n"
	require.NoError(t, os.WriteFile(rushFallbackPath, []byte(rushFallbackContent), 0o644))

	// Seed a foreign crush.toml WITHOUT any sentinel - should survive
	foreignCrushPath := filepath.Join(commandsDir, "foreign-crush.toml")
	foreignContent := "This is a foreign file without our sentinel\n"
	require.NoError(t, os.WriteFile(foreignCrushPath, []byte(foreignContent), 0o644))

	// Run gemini-del
	stderr := captureStderr(t, func() {
		require.NoError(t, runGeminiDel(dir))
	})

	// All files with either sentinel should be removed
	_, err := os.Stat(legacyCrushPath)
	assert.True(t, os.IsNotExist(err), "legacy crush.toml with legacy sentinel should be removed")

	_, err = os.Stat(legacyFallbackPath)
	assert.True(t, os.IsNotExist(err), "legacy crush-fallback.toml with legacy sentinel should be removed")

	_, err = os.Stat(rushPath)
	assert.True(t, os.IsNotExist(err), "rush.toml with new sentinel should be removed")

	_, err = os.Stat(rushFallbackPath)
	assert.True(t, os.IsNotExist(err), "rush-fallback.toml with new sentinel should be removed")

	// Foreign file without sentinel should survive
	foreignData, err := os.ReadFile(foreignCrushPath)
	require.NoError(t, err)
	assert.Equal(t, foreignContent, string(foreignData), "foreign file without sentinel should survive")

	// Verify stderr mentions all removals
	assert.Contains(t, stderr, "removed")
	assert.Contains(t, stderr, "crush")
	assert.Contains(t, stderr, "rush")
}
