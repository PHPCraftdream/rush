package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClaudeDel_RemovesLegacyPrerenameInstall verifies that claude-del removes
// both legacy crush.md/crush-fallback.md files (from pre-rename installs) and
// the new rush.md/rush-fallback.md files, while leaving foreign files alone.
func TestClaudeDel_RemovesLegacyPrerenameInstall(t *testing.T) {
	dir := t.TempDir()
	cmdsDir := filepath.Join(dir, ".claude", "commands")
	require.NoError(t, os.MkdirAll(cmdsDir, 0o755))

	// Seed legacy crush.md with legacy sentinel
	legacyCrushPath := filepath.Join(cmdsDir, "crush.md")
	legacyCrushContent := "<!-- crush-slash-command:v1 -->\nlegacy crush command\n"
	require.NoError(t, os.WriteFile(legacyCrushPath, []byte(legacyCrushContent), 0o644))

	// Seed legacy crush-fallback.md with legacy sentinel
	legacyFallbackPath := filepath.Join(cmdsDir, "crush-fallback.md")
	legacyFallbackContent := "<!-- crush-slash-command:v1 -->\nlegacy fallback command\n"
	require.NoError(t, os.WriteFile(legacyFallbackPath, []byte(legacyFallbackContent), 0o644))

	// Seed rush.md with new sentinel
	rushPath := filepath.Join(cmdsDir, "rush.md")
	rushContent := "<!-- rush-slash-command:v1 -->\nnew rush command\n"
	require.NoError(t, os.WriteFile(rushPath, []byte(rushContent), 0o644))

	// Seed rush-fallback.md with new sentinel
	rushFallbackPath := filepath.Join(cmdsDir, "rush-fallback.md")
	rushFallbackContent := "<!-- rush-slash-command:v1 -->\nnew rush fallback command\n"
	require.NoError(t, os.WriteFile(rushFallbackPath, []byte(rushFallbackContent), 0o644))

	// Seed a foreign crush.md WITHOUT any sentinel - should survive
	foreignCrushPath := filepath.Join(cmdsDir, "foreign-crush.md")
	foreignContent := "This is a foreign file without our sentinel\n"
	require.NoError(t, os.WriteFile(foreignCrushPath, []byte(foreignContent), 0o644))

	// Run claude-del
	stderr := captureStderr(t, func() {
		require.NoError(t, runClaudeDel(dir))
	})

	// All files with either sentinel should be removed
	_, err := os.Stat(legacyCrushPath)
	assert.True(t, os.IsNotExist(err), "legacy crush.md with legacy sentinel should be removed")

	_, err = os.Stat(legacyFallbackPath)
	assert.True(t, os.IsNotExist(err), "legacy crush-fallback.md with legacy sentinel should be removed")

	_, err = os.Stat(rushPath)
	assert.True(t, os.IsNotExist(err), "rush.md with new sentinel should be removed")

	_, err = os.Stat(rushFallbackPath)
	assert.True(t, os.IsNotExist(err), "rush-fallback.md with new sentinel should be removed")

	// Foreign file without sentinel should survive
	foreignData, err := os.ReadFile(foreignCrushPath)
	require.NoError(t, err)
	assert.Equal(t, foreignContent, string(foreignData), "foreign file without sentinel should survive")

	// Verify stderr mentions all removals
	assert.Contains(t, stderr, "removed")
	assert.Contains(t, stderr, "crush.md")
	assert.Contains(t, stderr, "crush-fallback.md")
	assert.Contains(t, stderr, "rush.md")
	assert.Contains(t, stderr, "rush-fallback.md")
}

// TestClaudeDel_PreservesForeignFileAtLegacyPath verifies that claude-del
// refuses to delete a foreign file at the legacy crush.md path if it doesn't
// contain our sentinel.
func TestClaudeDel_PreservesForeignFileAtLegacyPath(t *testing.T) {
	dir := t.TempDir()
	cmdsDir := filepath.Join(dir, ".claude", "commands")
	require.NoError(t, os.MkdirAll(cmdsDir, 0o755))

	// Seed a foreign crush.md WITHOUT any sentinel - should survive
	foreignCrushPath := filepath.Join(cmdsDir, "crush.md")
	foreignContent := "This is a foreign file without our sentinel\n"
	require.NoError(t, os.WriteFile(foreignCrushPath, []byte(foreignContent), 0o644))

	// Run claude-del
	stderr := captureStderr(t, func() {
		require.NoError(t, runClaudeDel(dir))
	})

	// Foreign file without sentinel should survive
	foreignData, err := os.ReadFile(foreignCrushPath)
	require.NoError(t, err)
	assert.Equal(t, foreignContent, string(foreignData), "foreign file without sentinel should survive")

	// Verify stderr mentions refusal to delete
	assert.Contains(t, stderr, "refusing to delete")
	assert.Contains(t, stderr, "crush.md")
	assert.Contains(t, stderr, "missing sentinel")
}
