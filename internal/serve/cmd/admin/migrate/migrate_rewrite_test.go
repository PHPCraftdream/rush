package migrate

// Tests for the legacy global/system path resolution and the in-place legacy-content rewrite of a migrated config (disabled_skills, path fields, dry-run reporting, true-negative). Split out of migrate_test.go to stay under the 900-line file cap.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLegacySystemConfigPath tests legacySystemConfigPath's Unix/Windows
// gating without touching the real filesystem: on Windows it must return
// empty (mirroring config.SystemConfig()'s empty return there, since no
// system-wide config location exists), and on Unix it must return the
// well-known legacy path.
func TestLegacySystemConfigPath(t *testing.T) {
	got := legacySystemConfigPath()
	if runtime.GOOS == "windows" {
		assert.Equal(t, "", got, "should be empty on Windows: no system-wide config location exists")
	} else {
		assert.Equal(t, "/etc/crush/crush.json", got)
	}
}

// TestMigrateSystemConfigSkippedWhenPathsEmpty tests that runMigrate's
// system-config migration step is skipped cleanly (no panic, no crash on
// filepath.Dir("")) when the legacy/current system paths are unavailable,
// which is the real, permanent state on Windows (config.SystemConfig()
// returns "" there — see internal/config/domains/datadir/systemconfig_windows.go) and is
// reproduced directly here rather than by mocking package-level path
// resolution, since runMigrate's system-config block already treats an
// empty legacy or current path as "skip" by construction.
func TestMigrateSystemConfigSkippedWhenPathsEmpty(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	root := t.TempDir()

	if runtime.GOOS != "windows" {
		t.Skip("this test exercises the empty-path skip branch, which is only naturally reachable on Windows; on Unix legacySystemConfigPath()/config.SystemConfig() are non-empty and the branch runs (covered by other tests' passing runs against isolated temp dirs, not the real /etc/ path)")
	}

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{root})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	// No system config section should appear in the output at all.
	assert.NotContains(t, output, "system config:")
}

// TestLegacyGlobalPathEnvPrecedence tests env var precedence for legacy global paths.
func TestLegacyGlobalPathEnvPrecedence(t *testing.T) {
	t.Run("RUSH_GLOBAL_* wins over XDG", func(t *testing.T) {
		// Unset CRUSH_* vars
		t.Setenv("CRUSH_GLOBAL_CONFIG", "")
		t.Setenv("CRUSH_GLOBAL_DATA", "")

		// Set RUSH_* vars
		configDir := t.TempDir()
		dataDir := t.TempDir()
		t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
		t.Setenv("RUSH_GLOBAL_DATA", dataDir)
		t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-config")
		t.Setenv("XDG_DATA_HOME", "/tmp/xdg-data")

		configPath := legacyGlobalConfigPath()
		dataPath := legacyGlobalDataPath()

		assert.True(t, strings.Contains(configPath, configDir), "should use RUSH_GLOBAL_CONFIG")
		assert.True(t, strings.Contains(dataPath, dataDir), "should use RUSH_GLOBAL_DATA")
		assert.Contains(t, configPath, "crush.json")
		assert.Contains(t, dataPath, "crush.json")
	})

	t.Run("CRUSH_GLOBAL_* wins over RUSH_GLOBAL_*", func(t *testing.T) {
		// Set both CRUSH_* and RUSH_* vars
		crushConfig := t.TempDir()
		crushData := t.TempDir()
		rushConfig := t.TempDir()
		rushData := t.TempDir()

		t.Setenv("CRUSH_GLOBAL_CONFIG", crushConfig)
		t.Setenv("CRUSH_GLOBAL_DATA", crushData)
		t.Setenv("RUSH_GLOBAL_CONFIG", rushConfig)
		t.Setenv("RUSH_GLOBAL_DATA", rushData)
		t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-config")
		t.Setenv("XDG_DATA_HOME", "/tmp/xdg-data")

		configPath := legacyGlobalConfigPath()
		dataPath := legacyGlobalDataPath()

		assert.True(t, strings.Contains(configPath, crushConfig), "should use CRUSH_GLOBAL_CONFIG over RUSH")
		assert.True(t, strings.Contains(dataPath, crushData), "should use CRUSH_GLOBAL_DATA over RUSH")
		assert.Contains(t, configPath, "crush.json")
		assert.Contains(t, dataPath, "crush.json")
	})
}

// TestMigrateRewritesLegacyDisabledSkills tests that a migrated rush.json
// containing legacy-named disabled_skills entries (the pre-rename builtin
// skill IDs "crush-config"/"crush-hooks") gets them rewritten to their
// current names ("rush-config"/"rush-hooks"), so internal/skills.Filter's
// literal string comparison keeps matching and the skill stays disabled.
func TestMigrateRewritesLegacyDisabledSkills(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	crushFile := filepath.Join(tmpDir, "crush.json")
	content := `{"options":{"disabled_skills":["crush-config","crush-hooks","jq","my-custom-skill"]}}`
	require.NoError(t, os.WriteFile(crushFile, []byte(content), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	rushFile := filepath.Join(tmpDir, "rush.json")
	raw, err := os.ReadFile(rushFile)
	require.NoError(t, err)

	var parsed struct {
		Options struct {
			DisabledSkills []string `json:"disabled_skills"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))

	assert.ElementsMatch(t, []string{"rush-config", "rush-hooks", "jq", "my-custom-skill"}, parsed.Options.DisabledSkills)
	assert.Contains(t, output, `disabled_skills: "crush-config" -> "rush-config"`)
	assert.Contains(t, output, `disabled_skills: "crush-hooks" -> "rush-hooks"`)
}

// TestMigrateRewritesLegacyPathFields tests that skills_paths,
// global_context_paths, and data_directory entries referencing a ".crush"
// path segment or the old "CRUSH.md" context-file name get rewritten to
// their Rush equivalents.
func TestMigrateRewritesLegacyPathFields(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	crushFile := filepath.Join(tmpDir, "crush.json")
	content := `{
  "options": {
    "skills_paths": [".crush/skills", "./skills", "~/.config/crush/skills"],
    "global_context_paths": ["~/.config/crush/CRUSH.md", "AGENTS.md"],
    "data_directory": ".crush"
  }
}`
	require.NoError(t, os.WriteFile(crushFile, []byte(content), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	rushFile := filepath.Join(tmpDir, "rush.json")
	raw, err := os.ReadFile(rushFile)
	require.NoError(t, err)

	var parsed struct {
		Options struct {
			SkillsPaths        []string `json:"skills_paths"`
			GlobalContextPaths []string `json:"global_context_paths"`
			DataDirectory      string   `json:"data_directory"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))

	assert.Equal(t, []string{".rush/skills", "./skills", "~/.config/rush/skills"}, parsed.Options.SkillsPaths)
	assert.Equal(t, []string{"~/.config/rush/RUSH.md", "AGENTS.md"}, parsed.Options.GlobalContextPaths)
	assert.Equal(t, ".rush", parsed.Options.DataDirectory)

	assert.Contains(t, output, "rewrote")
	assert.Contains(t, output, "(content inside migrated config)")
}

// TestMigrateRewriteDryRunReportsWithoutTouching tests that --dry-run
// reports what content WOULD be rewritten without modifying any file, same
// discipline as every other operation in this command.
func TestMigrateRewriteDryRunReportsWithoutTouching(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	crushFile := filepath.Join(tmpDir, "crush.json")
	content := `{"options":{"disabled_skills":["crush-config"],"skills_paths":[".crush/skills"]}}`
	require.NoError(t, os.WriteFile(crushFile, []byte(content), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	require.NoError(t, migrateCmd.Flags().Set("dry-run", "true"))
	defer migrateCmd.Flags().Set("dry-run", "false")
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	// Nothing renamed on disk.
	_, statErr := os.Stat(crushFile)
	require.NoError(t, statErr, "crush.json should still exist in dry-run")
	_, statErr = os.Stat(filepath.Join(tmpDir, "rush.json"))
	assert.True(t, os.IsNotExist(statErr), "rush.json should not exist in dry-run")

	// Content on disk is untouched.
	raw, err := os.ReadFile(crushFile)
	require.NoError(t, err)
	assert.Equal(t, content, string(raw))

	// But the report mentions what would be rewritten.
	assert.Contains(t, output, "would rewrite")
	assert.Contains(t, output, `disabled_skills: "crush-config" -> "rush-config"`)
	assert.Contains(t, output, `skills_paths: ".crush/skills" -> ".rush/skills"`)
}

// TestMigrateCleanConfigContentUnchanged tests that a migrated rush.json
// with no legacy-named field values (already-current content) is left
// byte-identical except for whatever the rename step itself already did -
// i.e. the content rewrite step must be a true no-op, not just "no visible
// difference", when there is nothing to fix.
func TestMigrateCleanConfigContentUnchanged(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	crushFile := filepath.Join(tmpDir, "crush.json")
	// Already-current content: no legacy skill IDs, no .crush path
	// segments, no CRUSH.md - nothing here should trigger a rewrite.
	content := `{"options":{"disabled_skills":["rush-config"],"skills_paths":[".rush/skills"],"data_directory":".rush","debug":true}}`
	require.NoError(t, os.WriteFile(crushFile, []byte(content), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	rushFile := filepath.Join(tmpDir, "rush.json")
	raw, err := os.ReadFile(rushFile)
	require.NoError(t, err)

	// Byte-identical: the rename step alone does not touch content, and
	// the rewrite step found nothing to change.
	assert.Equal(t, content, string(raw))
	assert.NotContains(t, output, "rewrote")
	assert.NotContains(t, output, "would rewrite")
}
