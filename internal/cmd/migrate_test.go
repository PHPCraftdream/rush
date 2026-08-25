package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

	dirStatus, innerStatus, _, _, _ := migrateGlobalLocation(testCmd, legacyConfig, currentPath, false, "test global:")

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

// TestMigrateKnownArtifactsProjectDir tests that a pre-existing crush.db and
// logs/crush.log inside a project-level .crush/ are renamed to rush.db and
// logs/rush.log alongside the directory migration, so session history and
// logs remain reachable at the paths the app now looks for them under.
func TestMigrateKnownArtifactsProjectDir(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	crushDir := filepath.Join(tmpDir, ".crush")
	require.NoError(t, os.MkdirAll(crushDir, 0o755))

	dbContent := []byte("sqlite-session-history-bytes")
	require.NoError(t, os.WriteFile(filepath.Join(crushDir, "crush.db"), dbContent, 0o644))

	logsDir := filepath.Join(crushDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o755))
	logContent := []byte("log line 1\nlog line 2\n")
	require.NoError(t, os.WriteFile(filepath.Join(logsDir, "crush.log"), logContent, 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	rushDir := filepath.Join(tmpDir, ".rush")

	// crush.db -> rush.db, content preserved, old name gone.
	dbGot, err := os.ReadFile(filepath.Join(rushDir, "rush.db"))
	require.NoError(t, err, "rush.db should exist")
	assert.Equal(t, dbContent, dbGot)
	_, err = os.Stat(filepath.Join(rushDir, "crush.db"))
	assert.True(t, os.IsNotExist(err), "crush.db should be gone")

	// logs/crush.log -> logs/rush.log, content preserved, old name gone,
	// "logs" subdirectory itself is NOT renamed.
	logGot, err := os.ReadFile(filepath.Join(rushDir, "logs", "rush.log"))
	require.NoError(t, err, "logs/rush.log should exist")
	assert.Equal(t, logContent, logGot)
	_, err = os.Stat(filepath.Join(rushDir, "logs", "crush.log"))
	assert.True(t, os.IsNotExist(err), "logs/crush.log should be gone")

	assert.Contains(t, output, "renamed project:")
}

// TestMigrateKnownArtifactsConflictRefused tests that a pre-existing rush.db
// at the target blocks the crush.db artifact rename (same conflict-refusal
// discipline as every other rename in this file): refuse and leave both
// files untouched, rather than clobbering the existing rush.db.
func TestMigrateKnownArtifactsConflictRefused(t *testing.T) {
	tmpDir := t.TempDir()

	crushDir := filepath.Join(tmpDir, ".crush")
	rushDir := filepath.Join(tmpDir, ".rush")
	require.NoError(t, os.MkdirAll(crushDir, 0o755))
	require.NoError(t, os.MkdirAll(rushDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(crushDir, "crush.db"), []byte("new-crush-db"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(rushDir, "rush.db"), []byte("existing-rush-db"), 0o644))

	// Simulate the post-rename state migrateKnownArtifacts expects for a
	// non-dry-run call: the directory itself has already been renamed
	// (crushDir here just stands in as the old-path argument, unused when
	// dryRun is false), so pass rushDir as both the artifact source and
	// target parent.
	var b bytes.Buffer
	testCmd := &cobra.Command{}
	testCmd.SetOut(&b)
	testCmd.SetErr(&b)
	require.NoError(t, os.WriteFile(filepath.Join(rushDir, "crush.db"), []byte("new-crush-db"), 0o644))
	renamed, refused, failed := migrateKnownArtifacts(testCmd, crushDir, rushDir, false, "project:")

	output := b.String()
	t.Logf("Output:\n%s", output)

	assert.Equal(t, 0, renamed)
	assert.Equal(t, 1, refused)
	assert.Equal(t, 0, failed)
	assert.Contains(t, output, "CONFLICT")

	// Both crush.db (source) and rush.db (target) remain untouched.
	crushContent, err := os.ReadFile(filepath.Join(rushDir, "crush.db"))
	require.NoError(t, err)
	assert.Equal(t, "new-crush-db", string(crushContent))

	rushContent, err := os.ReadFile(filepath.Join(rushDir, "rush.db"))
	require.NoError(t, err)
	assert.Equal(t, "existing-rush-db", string(rushContent))
}

// TestMigrateKnownArtifactsDryRun tests that a dry-run reports the artifact
// renames without touching anything, using the pre-rename (.crush) source
// paths since the directory rename itself hasn't happened yet.
func TestMigrateKnownArtifactsDryRun(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	crushDir := filepath.Join(tmpDir, ".crush")
	require.NoError(t, os.MkdirAll(crushDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(crushDir, "crush.db"), []byte("db"), 0o644))
	logsDir := filepath.Join(crushDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(logsDir, "crush.log"), []byte("log"), 0o644))

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

	// Nothing was actually renamed.
	_, err = os.Stat(filepath.Join(crushDir, "crush.db"))
	require.NoError(t, err, "crush.db should still exist")
	_, err = os.Stat(filepath.Join(logsDir, "crush.log"))
	require.NoError(t, err, "logs/crush.log should still exist")
	_, err = os.Stat(filepath.Join(tmpDir, ".rush"))
	assert.True(t, os.IsNotExist(err), ".rush should not exist in dry-run")

	assert.Contains(t, output, "would rename")
	assert.Contains(t, output, filepath.Join(crushDir, "crush.db"))
	assert.Contains(t, output, filepath.Join(tmpDir, ".rush", "rush.db"))
	assert.Contains(t, output, filepath.Join(logsDir, "crush.log"))
	assert.Contains(t, output, filepath.Join(tmpDir, ".rush", "logs", "rush.log"))
}

// TestMigrateRecursiveMigratesRoot tests that --recursive migrates the root
// directory's own .crush/crush.json (not just nested ones), regressing the
// bug where WalkDir's callback skipped path == root with a comment claiming
// it was "handled separately below" when nothing there actually did.
func TestMigrateRecursiveMigratesRoot(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	root := t.TempDir()

	// Seed .crush/crush.json at the root itself.
	rootCrushDir := filepath.Join(root, ".crush")
	rootCrushInner := filepath.Join(rootCrushDir, "crush.json")
	require.NoError(t, os.MkdirAll(rootCrushDir, 0o755))
	require.NoError(t, os.WriteFile(rootCrushInner, []byte(`{"root": true}`), 0o644))

	// Seed root-level crush.json too.
	rootCrushFile := filepath.Join(root, "crush.json")
	require.NoError(t, os.WriteFile(rootCrushFile, []byte(`{"root_file": true}`), 0o644))

	// Seed a nested .crush/crush.json in a subdirectory.
	nestedCrushDir := filepath.Join(root, "nested", ".crush")
	nestedCrushInner := filepath.Join(nestedCrushDir, "crush.json")
	require.NoError(t, os.MkdirAll(nestedCrushDir, 0o755))
	require.NoError(t, os.WriteFile(nestedCrushInner, []byte(`{"nested": true}`), 0o644))

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

	// Root .crush -> .rush, inner config migrated.
	rootRushInner := filepath.Join(root, ".rush", "rush.json")
	rootContent, err := os.ReadFile(rootRushInner)
	require.NoError(t, err, "root .rush/rush.json should exist")
	assert.Equal(t, `{"root": true}`, string(rootContent))
	_, err = os.Stat(rootCrushDir)
	assert.True(t, os.IsNotExist(err), "root .crush should be gone")

	// Root crush.json -> rush.json.
	rootFileContent, err := os.ReadFile(filepath.Join(root, "rush.json"))
	require.NoError(t, err, "root rush.json should exist")
	assert.Equal(t, `{"root_file": true}`, string(rootFileContent))

	// Nested .crush -> .rush still works too.
	nestedRushInner := filepath.Join(root, "nested", ".rush", "rush.json")
	nestedContent, err := os.ReadFile(nestedRushInner)
	require.NoError(t, err, "nested .rush/rush.json should exist")
	assert.Equal(t, `{"nested": true}`, string(nestedContent))
	_, err = os.Stat(nestedCrushDir)
	assert.True(t, os.IsNotExist(err), "nested .crush should be gone")
}

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
// returns "" there — see internal/config/config_windows.go) and is
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

// TestMigrateGlobalMergesRemainingDirContents tests the P2 fix: when a
// global legacy directory (e.g. ~/.config/crush) still has other files
// (skills/, auth.json) after its crush.json was migrated via the
// file-level path (case 2 - target directory already exists), those
// remaining entries are now moved into the target directory too, instead
// of being left behind with just a generic "other files" notice.
func TestMigrateGlobalMergesRemainingDirContents(t *testing.T) {
	tmpDir := t.TempDir()

	// Legacy dir: crush.json + a skills/ subdir (with a file inside) + auth.json.
	legacyDir := filepath.Join(tmpDir, "crush")
	legacyConfig := filepath.Join(legacyDir, "crush.json")
	legacySkillsDir := filepath.Join(legacyDir, "skills")
	legacySkillFile := filepath.Join(legacySkillsDir, "my-skill.md")
	legacyAuth := filepath.Join(legacyDir, "auth.json")
	require.NoError(t, os.MkdirAll(legacySkillsDir, 0o755))
	require.NoError(t, os.WriteFile(legacyConfig, []byte(`{"legacy": true}`), 0o644))
	require.NoError(t, os.WriteFile(legacySkillFile, []byte("# my skill"), 0o644))
	require.NoError(t, os.WriteFile(legacyAuth, []byte(`{"token": "secret"}`), 0o644))

	// Target dir already exists (the app itself created it on a prior run),
	// forcing migrateGlobalLocation into case 2 (file-level migration).
	rushDir := filepath.Join(tmpDir, "rush")
	currentPath := filepath.Join(rushDir, "rush.json")
	require.NoError(t, os.MkdirAll(rushDir, 0o755))

	var b bytes.Buffer
	testCmd := &cobra.Command{}
	testCmd.SetOut(&b)
	testCmd.SetErr(&b)

	dirStatus, innerStatus, mergeRenamed, mergeRefused, mergeFailed := migrateGlobalLocation(testCmd, legacyConfig, currentPath, false, "test global:")

	output := b.String()
	t.Logf("Output:\n%s", output)

	assert.Equal(t, statusRenamed, dirStatus)
	assert.Equal(t, statusNone, innerStatus)
	assert.Equal(t, 2, mergeRenamed, "skills/ dir and auth.json should both be merged")
	assert.Equal(t, 0, mergeRefused)
	assert.Equal(t, 0, mergeFailed)

	// crush.json -> rush.json in the target dir.
	newConfigContent, err := os.ReadFile(filepath.Join(rushDir, "rush.json"))
	require.NoError(t, err)
	assert.Equal(t, `{"legacy": true}`, string(newConfigContent))

	// skills/my-skill.md moved into the target dir intact.
	movedSkillFile := filepath.Join(rushDir, "skills", "my-skill.md")
	skillContent, err := os.ReadFile(movedSkillFile)
	require.NoError(t, err)
	assert.Equal(t, "# my skill", string(skillContent))

	// auth.json moved into the target dir intact.
	movedAuth := filepath.Join(rushDir, "auth.json")
	authContent, err := os.ReadFile(movedAuth)
	require.NoError(t, err)
	assert.Equal(t, `{"token": "secret"}`, string(authContent))

	// Legacy directory is now fully empty and was removed.
	_, err = os.Stat(legacyDir)
	assert.True(t, os.IsNotExist(err), "legacy dir should be removed once fully merged")

	assert.Contains(t, output, "(remaining item in migrated directory)")
}

// TestMigrateGlobalMergeRemainingPerItemConflict tests that a genuine
// per-item name conflict during the remaining-contents merge refuses only
// that one item (leaving both copies untouched) while every other,
// non-conflicting item still merges - and that the final notice names the
// refused item explicitly rather than a generic "other files" message.
func TestMigrateGlobalMergeRemainingPerItemConflict(t *testing.T) {
	tmpDir := t.TempDir()

	legacyDir := filepath.Join(tmpDir, "crush")
	legacyConfig := filepath.Join(legacyDir, "crush.json")
	legacyAuth := filepath.Join(legacyDir, "auth.json")
	legacySkillsDir := filepath.Join(legacyDir, "skills")
	require.NoError(t, os.MkdirAll(legacySkillsDir, 0o755))
	require.NoError(t, os.WriteFile(legacyConfig, []byte(`{"legacy": true}`), 0o644))
	require.NoError(t, os.WriteFile(legacyAuth, []byte("legacy-auth-bytes"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(legacySkillsDir, "s.md"), []byte("skill"), 0o644))

	// Target dir already exists AND already has its own auth.json - this
	// one item must be refused, while skills/ (no name conflict) still merges.
	rushDir := filepath.Join(tmpDir, "rush")
	currentPath := filepath.Join(rushDir, "rush.json")
	require.NoError(t, os.MkdirAll(rushDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rushDir, "auth.json"), []byte("existing-rush-auth"), 0o644))

	var b bytes.Buffer
	testCmd := &cobra.Command{}
	testCmd.SetOut(&b)
	testCmd.SetErr(&b)

	dirStatus, innerStatus, mergeRenamed, mergeRefused, mergeFailed := migrateGlobalLocation(testCmd, legacyConfig, currentPath, false, "test global:")

	output := b.String()
	t.Logf("Output:\n%s", output)

	assert.Equal(t, statusRenamed, dirStatus)
	assert.Equal(t, statusNone, innerStatus)
	assert.Equal(t, 1, mergeRenamed, "skills/ should merge")
	assert.Equal(t, 1, mergeRefused, "auth.json should be refused due to conflict")
	assert.Equal(t, 0, mergeFailed)

	// skills/ merged.
	_, err := os.Stat(filepath.Join(rushDir, "skills", "s.md"))
	require.NoError(t, err, "skills/s.md should have merged")

	// Both auth.json copies remain untouched (neither clobbered).
	legacyAuthContent, err := os.ReadFile(legacyAuth)
	require.NoError(t, err, "legacy auth.json should still exist, untouched")
	assert.Equal(t, "legacy-auth-bytes", string(legacyAuthContent))

	targetAuthContent, err := os.ReadFile(filepath.Join(rushDir, "auth.json"))
	require.NoError(t, err)
	assert.Equal(t, "existing-rush-auth", string(targetAuthContent))

	// Legacy directory still exists (auth.json left behind) - not removed.
	_, err = os.Stat(legacyDir)
	require.NoError(t, err, "legacy dir should still exist since auth.json was refused")

	// The final notice names the specific refused item, not a generic message.
	assert.Contains(t, output, "CONFLICT")
	assert.Contains(t, output, "auth.json")
	assert.Contains(t, output, "still contains 1 item(s) that were NOT merged due to name conflicts")
}

// TestMigrateManualFollowUpReportsStaleCrushEnvVars tests the P2/P3 fix:
// runMigrate's final "Manual follow-up needed" section scans the actual
// process environment for CRUSH_*-prefixed variables that migrate does NOT
// already handle itself, and lists them so the user knows to update their
// shell profile - while variables migrate DOES already account for
// (CRUSH_GLOBAL_CONFIG, CRUSH_GLOBAL_DATA, used internally as legacy
// fallbacks) are not re-listed as if they were a gap.
func TestMigrateManualFollowUpReportsStaleCrushEnvVars(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	root := t.TempDir()

	t.Setenv("CRUSH_SOMETHING_MADE_UP", "some-value")

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{root})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	assert.Contains(t, output, "Manual follow-up needed:")
	assert.Contains(t, output, "CRUSH_SOMETHING_MADE_UP")
	assert.Contains(t, output, "*-del")

	// CRUSH_GLOBAL_CONFIG/CRUSH_GLOBAL_DATA are handled by migrate itself
	// (as legacy-lookup fallbacks) and must NOT be reported as a gap, even
	// though isolateGlobalPaths sets RUSH_* (not CRUSH_*) equivalents here -
	// this assertion documents that if a real user DID have the CRUSH_*
	// forms set, they still wouldn't be listed.
	t.Setenv("CRUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("CRUSH_GLOBAL_DATA", t.TempDir())

	var b2 bytes.Buffer
	migrateCmd.SetOut(&b2)
	migrateCmd.SetErr(&b2)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err = migrateCmd.RunE(migrateCmd, []string{root})
	require.NoError(t, err)

	output2 := b2.String()
	t.Logf("Output2:\n%s", output2)

	assert.Contains(t, output2, "Manual follow-up needed:")
	assert.NotContains(t, output2, "CRUSH_GLOBAL_CONFIG")
	assert.NotContains(t, output2, "CRUSH_GLOBAL_DATA")
	// The unrelated stale var set earlier in this test is still reported.
	assert.Contains(t, output2, "CRUSH_SOMETHING_MADE_UP")
}

// TestMigrateManualFollowUpNoStaleVars tests that when no stray CRUSH_*
// variables are set, the report says so explicitly rather than silently
// omitting the section.
func TestMigrateManualFollowUpNoStaleVars(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	root := t.TempDir()

	// Best-effort: ensure no CRUSH_* leaks in from the outer test environment.
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(name, "CRUSH_") {
			t.Setenv(name, "")
		}
	}

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{root})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	assert.Contains(t, output, "Manual follow-up needed:")
	assert.Contains(t, output, "No stray CRUSH_* environment variables detected")
	assert.Contains(t, output, "*-del")
}

// TestMigrateUnrelatedCrushSubstringNotTouched is a true-negative test
// proving the rewrite step is NOT a blind find-and-replace: a field that
// isn't one of the four targeted fields, but happens to contain the
// substring "crush" in a way that is neither a skill-ID exact match nor a
// path segment boundary, must be left completely untouched. This also
// covers the hook-command-string gap noted in the task: a hooks/command
// style value containing "crush" as part of an arbitrary string is out of
// scope and must not be rewritten.
func TestMigrateUnrelatedCrushSubstringNotTouched(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	crushFile := filepath.Join(tmpDir, "crush.json")
	content := `{"options":{"initialize_as":"notcrushbar.md","skills_paths":["/foo/notcrushbar/skills"]},"description":"a tool that crushes rocks","hooks":{"pre":{"command":"run-crush-linter --strict"}}}`
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

	// Byte-identical: nothing in this content matches a rewrite rule.
	assert.Equal(t, content, string(raw))
	assert.NotContains(t, output, "rewrote")
	assert.NotContains(t, output, "would rewrite")
}
