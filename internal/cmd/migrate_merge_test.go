package cmd

// Migration tests for merging a legacy global directory into an existing current one: remaining entries, per-item conflicts, artifact name mapping, and the stale-environment-variable follow-up report. Split out of migrate_test.go when the 1000-line file limit landed.

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

// TestMigrateGlobalMergeMapsKnownArtifactNames tests that mergeRemainingDirEntries
// (the per-item merge path used when the target directory already exists)
// applies the same known-artifact name mapping migrateKnownArtifacts uses
// for the whole-directory-rename path: a stranded crush.db lands as rush.db,
// and logs/crush.log lands as logs/rush.log, both in the target directory -
// not under their original crush-style names, which would make them
// invisible to the app even though migration reported success.
func TestMigrateGlobalMergeMapsKnownArtifactNames(t *testing.T) {
	tmpDir := t.TempDir()

	legacyDir := filepath.Join(tmpDir, "crush")
	legacyConfig := filepath.Join(legacyDir, "crush.json")
	legacyDB := filepath.Join(legacyDir, "crush.db")
	legacyLogsDir := filepath.Join(legacyDir, "logs")
	legacyLog := filepath.Join(legacyLogsDir, "crush.log")
	require.NoError(t, os.MkdirAll(legacyLogsDir, 0o755))
	require.NoError(t, os.WriteFile(legacyConfig, []byte(`{"legacy": true}`), 0o644))
	require.NoError(t, os.WriteFile(legacyDB, []byte("db-bytes"), 0o644))
	require.NoError(t, os.WriteFile(legacyLog, []byte("log-bytes"), 0o644))

	// Target dir already exists, forcing case 2 (file-level migration).
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
	assert.Equal(t, 2, mergeRenamed, "crush.db and logs/crush.log should both be merged")
	assert.Equal(t, 0, mergeRefused)
	assert.Equal(t, 0, mergeFailed)

	// crush.db landed as rush.db in the target dir (not crush.db).
	dbContent, err := os.ReadFile(filepath.Join(rushDir, "rush.db"))
	require.NoError(t, err, "rush.db should exist in target dir")
	assert.Equal(t, "db-bytes", string(dbContent))
	_, err = os.Stat(filepath.Join(rushDir, "crush.db"))
	assert.True(t, os.IsNotExist(err), "crush.db should NOT exist in target dir under its old name")

	// logs/crush.log landed as logs/rush.log in the target dir.
	logContent, err := os.ReadFile(filepath.Join(rushDir, "logs", "rush.log"))
	require.NoError(t, err, "logs/rush.log should exist in target dir")
	assert.Equal(t, "log-bytes", string(logContent))
	_, err = os.Stat(filepath.Join(rushDir, "logs", "crush.log"))
	assert.True(t, os.IsNotExist(err), "logs/crush.log should NOT exist in target dir under its old name")

	// Legacy directory fully merged and removed.
	_, err = os.Stat(legacyDir)
	assert.True(t, os.IsNotExist(err), "legacy dir should be removed once fully merged")
}

// TestMigrateGlobalMergeArtifactConflictRefusesByOriginalName tests that when
// the MAPPED target name for a known artifact already exists in the target
// directory (e.g. target already has rush.db), mergeRemainingDirEntries
// refuses only that one item - reporting the conflict using the item's
// ORIGINAL (crush-style) name, since that's the name the user will find on
// disk in the legacy directory - while other, unrelated stranded items
// still merge normally.
func TestMigrateGlobalMergeArtifactConflictRefusesByOriginalName(t *testing.T) {
	tmpDir := t.TempDir()

	legacyDir := filepath.Join(tmpDir, "crush")
	legacyConfig := filepath.Join(legacyDir, "crush.json")
	legacyDB := filepath.Join(legacyDir, "crush.db")
	legacyAuth := filepath.Join(legacyDir, "auth.json")
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	require.NoError(t, os.WriteFile(legacyConfig, []byte(`{"legacy": true}`), 0o644))
	require.NoError(t, os.WriteFile(legacyDB, []byte("legacy-db-bytes"), 0o644))
	require.NoError(t, os.WriteFile(legacyAuth, []byte("legacy-auth-bytes"), 0o644))

	// Target dir already has its own rush.db - the mapped target name for
	// crush.db - so that specific item must be refused, while auth.json
	// (no conflict) still merges.
	rushDir := filepath.Join(tmpDir, "rush")
	currentPath := filepath.Join(rushDir, "rush.json")
	require.NoError(t, os.MkdirAll(rushDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rushDir, "rush.db"), []byte("existing-rush-db"), 0o644))

	var b bytes.Buffer
	testCmd := &cobra.Command{}
	testCmd.SetOut(&b)
	testCmd.SetErr(&b)

	dirStatus, innerStatus, mergeRenamed, mergeRefused, mergeFailed := migrateGlobalLocation(testCmd, legacyConfig, currentPath, false, "test global:")

	output := b.String()
	t.Logf("Output:\n%s", output)

	assert.Equal(t, statusRenamed, dirStatus)
	assert.Equal(t, statusNone, innerStatus)
	assert.Equal(t, 1, mergeRenamed, "auth.json should merge")
	assert.Equal(t, 1, mergeRefused, "crush.db should be refused due to mapped-name conflict")
	assert.Equal(t, 0, mergeFailed)

	// auth.json merged normally.
	_, err := os.Stat(filepath.Join(rushDir, "auth.json"))
	require.NoError(t, err, "auth.json should have merged")

	// Both crush.db/rush.db copies remain untouched (neither clobbered).
	legacyDBContent, err := os.ReadFile(legacyDB)
	require.NoError(t, err, "legacy crush.db should still exist, untouched")
	assert.Equal(t, "legacy-db-bytes", string(legacyDBContent))

	targetDBContent, err := os.ReadFile(filepath.Join(rushDir, "rush.db"))
	require.NoError(t, err)
	assert.Equal(t, "existing-rush-db", string(targetDBContent))

	// The conflict is reported using the ORIGINAL name (crush.db), not the
	// mapped target name, so the user can find the file being discussed.
	assert.Contains(t, output, "CONFLICT")
	assert.Contains(t, output, legacyDB)
	assert.Contains(t, output, filepath.Join(rushDir, "rush.db"))

	// The final notice also names the refused item by its original name.
	assert.Contains(t, output, "still contains 1 item(s) that were NOT merged due to name conflicts")
	assert.Contains(t, output, "crush.db")

	// Legacy directory still exists (crush.db left behind) - not removed.
	_, err = os.Stat(legacyDir)
	require.NoError(t, err, "legacy dir should still exist since crush.db was refused")
}

// TestMigrateGlobalMergeArtifactDryRunReportsMappedName tests that a dry-run
// merge of a stranded known artifact reports the MAPPED target name (e.g.
// rush.db), not the original crush-style name, so --dry-run output
// accurately previews what a real run would produce.
//
// This calls mergeRemainingDirEntries directly rather than going through
// migrateGlobalLocation's case 2: that outer wrapper's dry-run path returns
// immediately after reporting the primary crush.json -> rush.json rename
// (see the `if dryRun { ... return ... }` right after the case-2 conflict
// check) and never actually calls into the merge step at all - a
// pre-existing limitation predating this fix, out of scope here. The merge
// step's own dry-run behavior (which this fix touches) is still fully
// exercised by calling it directly.
func TestMigrateGlobalMergeArtifactDryRunReportsMappedName(t *testing.T) {
	tmpDir := t.TempDir()

	legacyDir := filepath.Join(tmpDir, "crush")
	legacyDB := filepath.Join(legacyDir, "crush.db")
	legacyLogsDir := filepath.Join(legacyDir, "logs")
	legacyLog := filepath.Join(legacyLogsDir, "crush.log")
	require.NoError(t, os.MkdirAll(legacyLogsDir, 0o755))
	require.NoError(t, os.WriteFile(legacyDB, []byte("db-bytes"), 0o644))
	require.NoError(t, os.WriteFile(legacyLog, []byte("log-bytes"), 0o644))

	rushDir := filepath.Join(tmpDir, "rush")
	require.NoError(t, os.MkdirAll(rushDir, 0o755))

	var b bytes.Buffer
	testCmd := &cobra.Command{}
	testCmd.SetOut(&b)
	testCmd.SetErr(&b)

	renamed, refused, failed, refusedNames := mergeRemainingDirEntries(testCmd, legacyDir, rushDir, true, "test global:")

	output := b.String()
	t.Logf("Output:\n%s", output)

	assert.Equal(t, 2, renamed)
	assert.Equal(t, 0, refused)
	assert.Equal(t, 0, failed)
	assert.Empty(t, refusedNames)

	// Nothing actually moved.
	_, err := os.Stat(legacyDB)
	require.NoError(t, err, "crush.db should still exist in legacy dir")
	_, err = os.Stat(legacyLog)
	require.NoError(t, err, "logs/crush.log should still exist in legacy dir")
	_, err = os.Stat(filepath.Join(rushDir, "rush.db"))
	assert.True(t, os.IsNotExist(err), "rush.db should not have been created in dry-run")

	// Output reports the mapped target names, not the original names.
	assert.Contains(t, output, "would rename")
	assert.Contains(t, output, legacyDB)
	assert.Contains(t, output, filepath.Join(rushDir, "rush.db"))
	assert.Contains(t, output, legacyLog)
	assert.Contains(t, output, filepath.Join(rushDir, "logs", "rush.log"))
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
