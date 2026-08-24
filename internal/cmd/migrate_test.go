package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateGlobalPaths redirects all global config/data environment variables
// to empty temp directories so tests can never touch the operator's real
// global config (which holds live credentials).
func isolateGlobalPaths(t *testing.T) (configDir, dataDir string) {
	t.Helper()
	configDir = t.TempDir()
	dataDir = t.TempDir()
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	return configDir, dataDir
}

// TestMigrateSingleDir tests basic single-directory migration.
func TestMigrateSingleDir(t *testing.T) {
	configDir, dataDir := isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	// Seed .crush/crush.json (workspace config)
	crushDir := filepath.Join(tmpDir, ".crush")
	crushInnerFile := filepath.Join(crushDir, "crush.json")
	innerContent := `{"workspace": "inner"}`
	require.NoError(t, os.MkdirAll(crushDir, 0o755))
	require.NoError(t, os.WriteFile(crushInnerFile, []byte(innerContent), 0o644))

	// Seed crush.json (root config)
	crushFile := filepath.Join(tmpDir, "crush.json")
	rootContent := `{"root": "config"}`
	require.NoError(t, os.WriteFile(crushFile, []byte(rootContent), 0o644))

	// Run migrate
	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	// Verify global dirs stayed empty
	configEntries, _ := os.ReadDir(configDir)
	dataEntries, _ := os.ReadDir(dataDir)
	assert.Empty(t, configEntries, "global config dir should be empty")
	assert.Empty(t, dataEntries, "global data dir should be empty")

	// Verify .crush is gone and .rush exists
	_, err = os.Stat(crushDir)
	assert.True(t, os.IsNotExist(err), ".crush should not exist")

	rushDir := filepath.Join(tmpDir, ".rush")
	rushInfo, err := os.Stat(rushDir)
	require.NoError(t, err, ".rush should exist")
	assert.True(t, rushInfo.IsDir(), ".rush should be a directory")

	// Verify .rush/rush.json exists with original inner content
	rushInnerFile := filepath.Join(rushDir, "rush.json")
	rushInnerContent, err := os.ReadFile(rushInnerFile)
	require.NoError(t, err)
	assert.Equal(t, innerContent, string(rushInnerContent))

	// Verify crush.json is gone
	_, err = os.Stat(crushFile)
	assert.True(t, os.IsNotExist(err), "crush.json should not exist")

	// Verify rush.json exists with original root content
	rushFile := filepath.Join(tmpDir, "rush.json")
	rushRootContent, err := os.ReadFile(rushFile)
	require.NoError(t, err)
	assert.Equal(t, rootContent, string(rushRootContent))

	// Verify output contains all three rename lines
	assert.Contains(t, output, "renamed project:")
	assert.Contains(t, output, crushDir)
	assert.Contains(t, output, rushDir)
	assert.Contains(t, output, "(config inside migrated directory)")
	assert.Contains(t, output, crushFile)
	assert.Contains(t, output, rushFile)
}

// TestMigrateRecursiveIgnoresNodeModules tests that recursive migration skips node_modules.
func TestMigrateRecursiveIgnoresNodeModules(t *testing.T) {
	configDir, dataDir := isolateGlobalPaths(t)
	root := t.TempDir()

	// Seed a/.crush/crush.json
	aCrushDir := filepath.Join(root, "a", ".crush")
	aCrushFile := filepath.Join(aCrushDir, "crush.json")
	require.NoError(t, os.MkdirAll(aCrushDir, 0o755))
	require.NoError(t, os.WriteFile(aCrushFile, []byte(`{"a": true}`), 0o644))

	// Seed node_modules/.crush/crush.json (should be ignored)
	nmCrushDir := filepath.Join(root, "node_modules", ".crush")
	nmCrushFile := filepath.Join(nmCrushDir, "crush.json")
	require.NoError(t, os.MkdirAll(nmCrushDir, 0o755))
	require.NoError(t, os.WriteFile(nmCrushFile, []byte(`{"nm": true}`), 0o644))

	// Seed node_modules/crush.json (should be ignored)
	nmCrushRoot := filepath.Join(root, "node_modules", "crush.json")
	require.NoError(t, os.WriteFile(nmCrushRoot, []byte(`{"nm_root": true}`), 0o644))

	// Seed b/crush.json
	bCrushFile := filepath.Join(root, "b", "crush.json")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "b"), 0o755))
	require.NoError(t, os.WriteFile(bCrushFile, []byte(`{"b": true}`), 0o644))

	// Run migrate with recursive flag
	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	require.NoError(t, migrateCmd.Flags().Set("recursive", "true"))
	defer migrateCmd.Flags().Set("recursive", "false")
	err := migrateCmd.RunE(migrateCmd, []string{root})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	// Verify global dirs stayed empty
	configEntries, _ := os.ReadDir(configDir)
	dataEntries, _ := os.ReadDir(dataDir)
	assert.Empty(t, configEntries, "global config dir should be empty")
	assert.Empty(t, dataEntries, "global data dir should be empty")

	// Verify a/.rush/rush.json exists
	aRushFile := filepath.Join(root, "a", ".rush", "rush.json")
	content, err := os.ReadFile(aRushFile)
	require.NoError(t, err)
	assert.Equal(t, `{"a": true}`, string(content))

	// Verify b/rush.json exists
	bRushFile := filepath.Join(root, "b", "rush.json")
	content, err = os.ReadFile(bRushFile)
	require.NoError(t, err)
	assert.Equal(t, `{"b": true}`, string(content))

	// Verify node_modules/.crush/crush.json STILL EXISTS UNTOUCHED
	nmContent, err := os.ReadFile(nmCrushFile)
	require.NoError(t, err)
	assert.Equal(t, `{"nm": true}`, string(nmContent))

	// Verify node_modules/crush.json STILL EXISTS UNTOUCHED
	nmRootContent, err := os.ReadFile(nmCrushRoot)
	require.NoError(t, err)
	assert.Equal(t, `{"nm_root": true}`, string(nmRootContent))
}

// TestMigrateConflictDirRefused tests that directory conflicts are refused.
func TestMigrateConflictDirRefused(t *testing.T) {
	configDir, dataDir := isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	// Seed both .crush and .rush directories
	crushDir := filepath.Join(tmpDir, ".crush")
	rushDir := filepath.Join(tmpDir, ".rush")
	require.NoError(t, os.MkdirAll(crushDir, 0o755))
	require.NoError(t, os.MkdirAll(rushDir, 0o755))

	// Add marker files
	crushMarker := filepath.Join(crushDir, "crush.marker")
	rushMarker := filepath.Join(rushDir, "rush.marker")
	require.NoError(t, os.WriteFile(crushMarker, []byte("crush"), 0o644))
	require.NoError(t, os.WriteFile(rushMarker, []byte("rush"), 0o644))

	// Run migrate
	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.Error(t, err, "should return error on conflict")

	output := b.String()
	t.Logf("Output:\n%s", output)

	// Verify global dirs stayed empty
	configEntries, _ := os.ReadDir(configDir)
	dataEntries, _ := os.ReadDir(dataDir)
	assert.Empty(t, configEntries, "global config dir should be empty")
	assert.Empty(t, dataEntries, "global data dir should be empty")

	// Verify both dirs still exist with unchanged markers
	crushMarkerContent, err := os.ReadFile(crushMarker)
	require.NoError(t, err)
	assert.Equal(t, "crush", string(crushMarkerContent))

	rushMarkerContent, err := os.ReadFile(rushMarker)
	require.NoError(t, err)
	assert.Equal(t, "rush", string(rushMarkerContent))

	// Verify output contains CONFLICT line
	assert.Contains(t, output, "CONFLICT")
	assert.Contains(t, output, crushDir)
	assert.Contains(t, output, rushDir)
}

// TestMigrateConflictFileRefused tests that file conflicts are refused and other items proceed.
func TestMigrateConflictFileRefused(t *testing.T) {
	t.Run("conflict refused", func(t *testing.T) {
		configDir, dataDir := isolateGlobalPaths(t)
		tmpDir := t.TempDir()

		// Seed both crush.json and rush.json with distinct contents
		crushFile := filepath.Join(tmpDir, "crush.json")
		rushFile := filepath.Join(tmpDir, "rush.json")
		require.NoError(t, os.WriteFile(crushFile, []byte(`{"old": true}`), 0o644))
		require.NoError(t, os.WriteFile(rushFile, []byte(`{"new": true}`), 0o644))

		// Run migrate
		var b bytes.Buffer
		migrateCmd.SetOut(&b)
		migrateCmd.SetErr(&b)
		migrateCmd.SetIn(bytes.NewReader(nil))
		err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
		require.Error(t, err, "should return error on conflict")

		output := b.String()
		t.Logf("Output:\n%s", output)

		// Verify global dirs stayed empty
		configEntries, _ := os.ReadDir(configDir)
		dataEntries, _ := os.ReadDir(dataDir)
		assert.Empty(t, configEntries, "global config dir should be empty")
		assert.Empty(t, dataEntries, "global data dir should be empty")

		// Verify both files still exist with unchanged contents
		crushContent, err := os.ReadFile(crushFile)
		require.NoError(t, err)
		assert.Equal(t, `{"old": true}`, string(crushContent))

		rushContent, err := os.ReadFile(rushFile)
		require.NoError(t, err)
		assert.Equal(t, `{"new": true}`, string(rushContent))

		// Verify output contains CONFLICT line
		assert.Contains(t, output, "CONFLICT")
		assert.Contains(t, output, crushFile)
		assert.Contains(t, output, rushFile)
	})

	t.Run("other items proceed despite file conflict", func(t *testing.T) {
		configDir, dataDir := isolateGlobalPaths(t)
		tmpDir := t.TempDir()

		// Seed .crush directory (clean, no .rush)
		crushDir := filepath.Join(tmpDir, ".crush")
		crushInnerFile := filepath.Join(crushDir, "crush.json")
		require.NoError(t, os.MkdirAll(crushDir, 0o755))
		require.NoError(t, os.WriteFile(crushInnerFile, []byte(`{"inner": true}`), 0o644))

		// Seed both crush.json and rush.json (conflicting)
		crushFile := filepath.Join(tmpDir, "crush.json")
		rushFile := filepath.Join(tmpDir, "rush.json")
		require.NoError(t, os.WriteFile(crushFile, []byte(`{"old": true}`), 0o644))
		require.NoError(t, os.WriteFile(rushFile, []byte(`{"new": true}`), 0o644))

		// Run migrate
		var b bytes.Buffer
		migrateCmd.SetOut(&b)
		migrateCmd.SetErr(&b)
		migrateCmd.SetIn(bytes.NewReader(nil))
		err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
		require.Error(t, err, "should return error on conflict")

		output := b.String()
		t.Logf("Output:\n%s", output)

		// Verify global dirs stayed empty
		configEntries, _ := os.ReadDir(configDir)
		dataEntries, _ := os.ReadDir(dataDir)
		assert.Empty(t, configEntries, "global config dir should be empty")
		assert.Empty(t, dataEntries, "global data dir should be empty")

		// Verify .crush WAS renamed to .rush even though file was refused
		_, err = os.Stat(crushDir)
		assert.True(t, os.IsNotExist(err), ".crush should not exist")

		newRushDir := filepath.Join(tmpDir, ".rush")
		rushInnerFile := filepath.Join(newRushDir, "rush.json")
		content, err := os.ReadFile(rushInnerFile)
		require.NoError(t, err)
		assert.Equal(t, `{"inner": true}`, string(content))

		// Verify conflicting files unchanged
		oldContent, err := os.ReadFile(crushFile)
		require.NoError(t, err)
		assert.Equal(t, `{"old": true}`, string(oldContent))

		newContent, err := os.ReadFile(rushFile)
		require.NoError(t, err)
		assert.Equal(t, `{"new": true}`, string(newContent))

		// Verify output shows both rename (dir) and conflict (file)
		assert.Contains(t, output, "renamed project:")
		assert.Contains(t, output, "CONFLICT")
	})
}

// TestMigrateDryRunChangesNothing tests that dry-run makes no changes.
func TestMigrateDryRunChangesNothing(t *testing.T) {
	configDir, dataDir := isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	// Seed .crush/crush.json and crush.json
	crushDir := filepath.Join(tmpDir, ".crush")
	crushInnerFile := filepath.Join(crushDir, "crush.json")
	innerContent := `{"inner": "test"}`
	require.NoError(t, os.MkdirAll(crushDir, 0o755))
	require.NoError(t, os.WriteFile(crushInnerFile, []byte(innerContent), 0o644))

	crushFile := filepath.Join(tmpDir, "crush.json")
	rootContent := `{"root": "test"}`
	require.NoError(t, os.WriteFile(crushFile, []byte(rootContent), 0o644))

	// Run migrate with dry-run flag
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

	// Verify global dirs stayed empty
	configEntries, _ := os.ReadDir(configDir)
	dataEntries, _ := os.ReadDir(dataDir)
	assert.Empty(t, configEntries, "global config dir should be empty")
	assert.Empty(t, dataEntries, "global data dir should be empty")

	// Verify .crush and crush.json still exist exactly as seeded
	_, err = os.Stat(crushDir)
	require.NoError(t, err, ".crush should still exist")

	content, err := os.ReadFile(crushInnerFile)
	require.NoError(t, err)
	assert.Equal(t, innerContent, string(content))

	_, err = os.Stat(crushFile)
	require.NoError(t, err, "crush.json should still exist")

	content, err = os.ReadFile(crushFile)
	require.NoError(t, err)
	assert.Equal(t, rootContent, string(content))

	// Verify .rush and rush.json do NOT exist
	_, err = os.Stat(filepath.Join(tmpDir, ".rush"))
	assert.True(t, os.IsNotExist(err), ".rush should not exist in dry-run")

	_, err = os.Stat(filepath.Join(tmpDir, "rush.json"))
	assert.True(t, os.IsNotExist(err), "rush.json should not exist in dry-run")

	// Verify output contains "would rename" lines
	assert.Contains(t, output, "would rename")
	assert.Contains(t, output, "dry-run: no changes were made")
}

// TestMigrateGlobalPaths tests global config and data migration.
func TestMigrateGlobalPaths(t *testing.T) {
	configDir, dataDir := isolateGlobalPaths(t)
	root := t.TempDir()

	// Seed global config and data
	configCrushFile := filepath.Join(configDir, "crush.json")
	configContent := `{"config": "global"}`
	require.NoError(t, os.WriteFile(configCrushFile, []byte(configContent), 0o644))

	dataCrushFile := filepath.Join(dataDir, "crush.json")
	dataContent := `{"data": "global"}`
	require.NoError(t, os.WriteFile(dataCrushFile, []byte(dataContent), 0o644))

	// Run migrate with empty project root
	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{root})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	// Verify config migrated
	configRushFile := filepath.Join(configDir, "rush.json")
	content, err := os.ReadFile(configRushFile)
	require.NoError(t, err)
	assert.Equal(t, configContent, string(content))

	_, err = os.Stat(configCrushFile)
	assert.True(t, os.IsNotExist(err), "config crush.json should be gone")

	// Verify data migrated
	dataRushFile := filepath.Join(dataDir, "rush.json")
	content, err = os.ReadFile(dataRushFile)
	require.NoError(t, err)
	assert.Equal(t, dataContent, string(content))

	_, err = os.Stat(dataCrushFile)
	assert.True(t, os.IsNotExist(err), "data crush.json should be gone")

	// Verify output contains global migration lines
	assert.Contains(t, output, "global config:")
	assert.Contains(t, output, "global data:")
}

// TestMigrateNothingToDo tests migration with nothing to migrate.
func TestMigrateNothingToDo(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	root := t.TempDir()

	// Run migrate on empty root
	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{root})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	// Verify output mentions nothing to migrate
	assert.Contains(t, output, "Nothing to migrate")
}

// TestMigrateGlobalDirLevelRename tests directory-level global migration.
func TestMigrateGlobalDirLevelRename(t *testing.T) {
	tmpDir := t.TempDir()

	// Build legacy dir structure: <tmp>/crush/crush.json + auth.json
	legacyDir := filepath.Join(tmpDir, "crush")
	legacyConfig := filepath.Join(legacyDir, "crush.json")
	authMarker := filepath.Join(legacyDir, "auth.json")
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	require.NoError(t, os.WriteFile(legacyConfig, []byte(`{"legacy": true}`), 0o644))
	require.NoError(t, os.WriteFile(authMarker, []byte(`{"auth": "data"}`), 0o644))

	// Set up current path: <tmp>/rush/rush.json (rush dir absent initially)
	rushDir := filepath.Join(tmpDir, "rush")
	currentPath := filepath.Join(rushDir, "rush.json")

	// Run migrateGlobalLocation directly
	var b bytes.Buffer
	testCmd := &cobra.Command{}
	testCmd.SetOut(&b)
	testCmd.SetErr(&b)

	dirStatus, innerStatus := migrateGlobalLocation(testCmd, legacyConfig, currentPath, false, "test global:")

	output := b.String()
	t.Logf("Output:\n%s", output)

	// Verify directory was renamed
	assert.Equal(t, statusRenamed, dirStatus)
	assert.Equal(t, statusRenamed, innerStatus)

	// Verify legacy dir gone
	_, err := os.Stat(legacyDir)
	assert.True(t, os.IsNotExist(err), "legacy dir should be gone")

	// Verify rush dir now exists
	rushDirInfo, err := os.Stat(rushDir)
	require.NoError(t, err, "rush dir should exist")
	assert.True(t, rushDirInfo.IsDir())

	// Verify inner config renamed
	newConfig := filepath.Join(rushDir, "rush.json")
	content, err := os.ReadFile(newConfig)
	require.NoError(t, err)
	assert.Equal(t, `{"legacy": true}`, string(content))

	// Verify old crush.json inside is gone
	_, err = os.Stat(filepath.Join(rushDir, "crush.json"))
	assert.True(t, os.IsNotExist(err), "inner crush.json should be gone")

	// Verify auth.json marker moved to new location
	newAuthMarker := filepath.Join(rushDir, "auth.json")
	authContent, err := os.ReadFile(newAuthMarker)
	require.NoError(t, err)
	assert.Equal(t, `{"auth": "data"}`, string(authContent))

	// Verify output
	assert.Contains(t, output, "renamed test global:")
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
