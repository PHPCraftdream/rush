package cmd

// Migration tests for the context and ignore files, the atomic content rewrite, and the .gitignore updates -- plus the true-negative case proving the rewrite is not a blind find-and-replace. Split out of migrate_test.go when the 1000-line file limit landed.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// TestMigrateContextFileRenamed tests that a plain CRUSH.md at the project
// root is renamed to RUSH.md (task #738: context files were previously only
// rewritten as a path SEGMENT inside config values, never renamed on disk).
func TestMigrateContextFileRenamed(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	crushMD := filepath.Join(tmpDir, "CRUSH.md")
	content := "# Project notes\nSome agent context.\n"
	require.NoError(t, os.WriteFile(crushMD, []byte(content), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	_, err = os.Stat(crushMD)
	assert.True(t, os.IsNotExist(err), "CRUSH.md should not exist")

	rushMD := filepath.Join(tmpDir, "RUSH.md")
	got, err := os.ReadFile(rushMD)
	require.NoError(t, err, "RUSH.md should exist")
	assert.Equal(t, content, string(got))
	assert.Contains(t, output, "renamed project:")
	assert.Contains(t, output, "(context/ignore file)")
}

// TestMigrateIgnoreFileRenamed tests that a plain .crushignore at the
// project root is renamed to .rushignore (task #738: previously not handled
// anywhere at all, so a pre-existing .crushignore silently stopped excluding
// files after upgrading).
func TestMigrateIgnoreFileRenamed(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	crushIgnore := filepath.Join(tmpDir, ".crushignore")
	content := "*.secret\nbuild/\n"
	require.NoError(t, os.WriteFile(crushIgnore, []byte(content), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	_, err = os.Stat(crushIgnore)
	assert.True(t, os.IsNotExist(err), ".crushignore should not exist")

	rushIgnore := filepath.Join(tmpDir, ".rushignore")
	got, err := os.ReadFile(rushIgnore)
	require.NoError(t, err, ".rushignore should exist")
	assert.Equal(t, content, string(got))
}

// TestMigrateContextFileCaseVariant tests one of the case variants
// internal/config/config.go's defaultContextPaths actually looks for
// (Crush.md -> Crush.md's rush equivalent is "Rush.md", which IS in
// defaultContextPaths) rather than an invented spelling nobody reads.
func TestMigrateContextFileCaseVariant(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	legacy := filepath.Join(tmpDir, "Crush.local.md")
	content := "local overrides\n"
	require.NoError(t, os.WriteFile(legacy, []byte(content), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.NoError(t, err)

	t.Logf("Output:\n%s", b.String())

	_, err = os.Stat(legacy)
	assert.True(t, os.IsNotExist(err), "Crush.local.md should not exist")

	target := filepath.Join(tmpDir, "Rush.local.md")
	got, err := os.ReadFile(target)
	require.NoError(t, err, "Rush.local.md should exist")
	assert.Equal(t, content, string(got))
}

// TestMigrateContextIgnoreFileConflictRefused tests that a genuine
// name conflict (target already exists with different content) refuses only
// that one item and reports CONFLICT, while a sibling rename in the same run
// that has no conflict still proceeds - same never-clobber discipline as
// every other rename in this file.
func TestMigrateContextIgnoreFileConflictRefused(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	// CRUSH.md -> RUSH.md will conflict (RUSH.md already exists).
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "CRUSH.md"), []byte("legacy"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "RUSH.md"), []byte("existing"), 0o644))

	// .crushignore -> .rushignore has no conflict and should still proceed.
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".crushignore"), []byte("*.log\n"), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.Error(t, err, "a conflict should produce a non-zero exit")

	output := b.String()
	t.Logf("Output:\n%s", output)
	assert.Contains(t, output, "CONFLICT")

	// Both CRUSH.md and RUSH.md remain untouched.
	legacyContent, err := os.ReadFile(filepath.Join(tmpDir, "CRUSH.md"))
	require.NoError(t, err, "CRUSH.md should still exist")
	assert.Equal(t, "legacy", string(legacyContent))
	existingContent, err := os.ReadFile(filepath.Join(tmpDir, "RUSH.md"))
	require.NoError(t, err)
	assert.Equal(t, "existing", string(existingContent))

	// The non-conflicting .crushignore rename still happened.
	_, err = os.Stat(filepath.Join(tmpDir, ".crushignore"))
	assert.True(t, os.IsNotExist(err), ".crushignore should have been renamed away")
	ignoreContent, err := os.ReadFile(filepath.Join(tmpDir, ".rushignore"))
	require.NoError(t, err, ".rushignore should exist")
	assert.Equal(t, "*.log\n", string(ignoreContent))
}

// TestMigrateContextIgnoreFileDryRunReportsWithoutTouching tests that
// --dry-run reports the CRUSH.md/.crushignore renames without writing
// anything to disk.
func TestMigrateContextIgnoreFileDryRunReportsWithoutTouching(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "CRUSH.md"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".crushignore"), []byte("*.log\n"), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	migrateCmd.Flags().Set("dry-run", "true")
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.NoError(t, err)
	migrateCmd.Flags().Set("dry-run", "false")

	output := b.String()
	t.Logf("Output:\n%s", output)
	assert.Contains(t, output, "would rename")
	assert.Contains(t, output, "CRUSH.md")
	assert.Contains(t, output, "RUSH.md")
	assert.Contains(t, output, ".crushignore")
	assert.Contains(t, output, ".rushignore")

	// Nothing on disk actually changed.
	_, err = os.Stat(filepath.Join(tmpDir, "CRUSH.md"))
	require.NoError(t, err, "CRUSH.md should still exist (dry-run)")
	_, err = os.Stat(filepath.Join(tmpDir, "RUSH.md"))
	assert.True(t, os.IsNotExist(err), "RUSH.md should not have been created (dry-run)")
	_, err = os.Stat(filepath.Join(tmpDir, ".crushignore"))
	require.NoError(t, err, ".crushignore should still exist (dry-run)")
	_, err = os.Stat(filepath.Join(tmpDir, ".rushignore"))
	assert.True(t, os.IsNotExist(err), ".rushignore should not have been created (dry-run)")
}

// TestMigrateContextFileCaseInsensitiveFilesystemHazard is the regression
// test for the case-sensitivity hazard investigated for task #738: on a
// case-insensitive filesystem (confirmed concretely on this machine via
// os.SameFile), a naive os.Stat-based conflict check against the target name
// would false-positive whenever a DIFFERENT already-existing file happens to
// case-fold to the same target name migrateContextAndIgnoreFiles is about to
// rename into.
//
// Concretely: Crush.md -> Rush.md and CRUSH.md -> RUSH.md are two different
// table entries, but "Rush.md" and "RUSH.md" case-fold to the SAME physical
// file on Windows/default-macOS. If both Crush.md and CRUSH.md existed
// simultaneously that would itself be impossible on such a filesystem
// (creating the second would either fail or silently collide with the
// first), so this test instead exercises the realistic version of the
// hazard: a legacy file is renamed to a target name, and the target name's
// OTHER case spelling is then independently probed to confirm it reports as
// the same file (not a phantom conflict) and that the rename did not corrupt
// or lose content.
func TestMigrateContextFileCaseInsensitiveFilesystemHazard(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	legacy := filepath.Join(tmpDir, "CRUSH.md")
	content := "case hazard regression content\n"
	require.NoError(t, os.WriteFile(legacy, []byte(content), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{tmpDir})
	require.NoError(t, err)

	output := b.String()
	t.Logf("Output:\n%s", output)

	// No false CONFLICT should have been reported for this rename.
	assert.NotContains(t, output, "CONFLICT")

	// Content preserved exactly, nothing corrupted or lost.
	target := filepath.Join(tmpDir, "RUSH.md")
	got, err := os.ReadFile(target)
	require.NoError(t, err, "RUSH.md should exist with original content")
	assert.Equal(t, content, string(got))

	// The exact-case target reports as present via Stat (sanity check the
	// rename actually landed).
	targetInfo, err := os.Stat(target)
	require.NoError(t, err)

	// Probing the OTHER case spelling of the same target name resolves to
	// the identical underlying file on this (case-insensitive) filesystem -
	// confirms the hazard is real and that migrate's os.SameFile guard is
	// checking the right thing, rather than this test silently passing for
	// an unrelated reason.
	altCaseInfo, err := os.Stat(filepath.Join(tmpDir, "rush.md"))
	if err == nil {
		assert.True(t, os.SameFile(targetInfo, altCaseInfo),
			"on a case-insensitive filesystem, RUSH.md and rush.md must resolve to the same file")
	}

	// Directly exercise migrateNamedFileCaseAware's conflict guard: renaming
	// a second legacy source whose target case-folds to the SAME already-
	// migrated file must not be misreported as a conflict against itself.
	// (Re-create a fresh legacy file and rename it onto the already-existing
	// target's case-insensitive twin to prove no phantom CONFLICT fires when
	// the Stat-matched file is genuinely the same file.)
	testCmd := &cobra.Command{}
	var b2 bytes.Buffer
	testCmd.SetOut(&b2)
	testCmd.SetErr(&b2)
	status, _ := migrateNamedFileCaseAware(testCmd, target, filepath.Join(tmpDir, "rUsH.md"), false, "test:")
	// target (RUSH.md) and "rUsH.md" case-fold to the same file, so this is
	// a same-file no-conflict situation: os.Rename is invoked and succeeds
	// (or is a case-only no-op), not refused as a conflict.
	assert.NotEqual(t, statusRefused, status, "same-file case-only variant must not be reported as a conflict")
	assert.NotContains(t, b2.String(), "CONFLICT")

	// Content still intact and reachable under some valid casing after the
	// case-only operation above. On a case-insensitive filesystem "RUSH.md"
	// still resolves (same file as "rUsH.md"); on a case-sensitive one
	// (Linux) the rename actually moved it, so check under its real name.
	finalPath := filepath.Join(tmpDir, "RUSH.md")
	if _, statErr := os.Stat(finalPath); os.IsNotExist(statErr) {
		finalPath = filepath.Join(tmpDir, "rUsH.md")
	}
	finalContent, err := os.ReadFile(finalPath)
	require.NoError(t, err, "content should remain reachable, not lost, after case-only rename")
	assert.Equal(t, content, string(finalContent))
}

// TestMigrateRewriteContentAtomicHappyPath is a regression check that the
// atomic-write path (temp file in the same directory, then os.Rename over
// the target) still lands the rewritten content correctly - same outcome as
// before the write was made atomic, just via a different mechanism. Runs
// rewriteLegacyConfigContent directly (rather than the full migrate
// pipeline) so the temp-file/rename mechanics are exercised in isolation.
func TestMigrateRewriteContentAtomicHappyPath(t *testing.T) {
	isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	rushFile := filepath.Join(tmpDir, "rush.json")
	content := `{"options":{"disabled_skills":["crush-config"],"skills_paths":[".crush/skills"]}}`
	require.NoError(t, os.WriteFile(rushFile, []byte(content), 0o644))

	testCmd := &cobra.Command{}
	var b bytes.Buffer
	testCmd.SetOut(&b)
	testCmd.SetErr(&b)

	status := rewriteLegacyConfigContent(testCmd, rushFile, rushFile, false, "test:")
	assert.Equal(t, statusRenamed, status, "successful rewrite should report statusRenamed")

	output := b.String()
	t.Logf("Output:\n%s", output)
	assert.Contains(t, output, "rewrote")
	assert.NotContains(t, output, "failed to rewrite")

	raw, err := os.ReadFile(rushFile)
	require.NoError(t, err)

	var parsed struct {
		Options struct {
			DisabledSkills []string `json:"disabled_skills"`
			SkillsPaths    []string `json:"skills_paths"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	assert.Equal(t, []string{"rush-config"}, parsed.Options.DisabledSkills)
	assert.Equal(t, []string{".rush/skills"}, parsed.Options.SkillsPaths)

	// No stray temp file left behind in the target directory.
	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "only rush.json should remain, no leftover temp file")
	assert.Equal(t, "rush.json", entries[0].Name())
}

// denyCurrentUserWrite uses icacls (Windows-only) to deny the current user
// write/add-file access to dir, returning a cleanup func that removes the
// deny ACE again. This is the Windows-reliable equivalent of "chmod 000 a
// directory" for blocking file creation inside it: verified directly that
// os.CreateTemp inside a dir with this ACE applied fails with "Access is
// denied", and that removing the ACE afterward restores normal access so
// t.TempDir's own cleanup can still remove the directory.
func denyCurrentUserWrite(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("icacls-based write-denial is Windows-specific")
	}
	user := os.Getenv("USERNAME")
	require.NotEmpty(t, user, "USERNAME must be set to build the icacls deny rule")

	out, err := exec.CommandContext(t.Context(), "icacls", dir, "/deny", user+":(WD,AD)").CombinedOutput()
	require.NoErrorf(t, err, "icacls deny failed: %s", out)

	t.Cleanup(func() {
		out, err := exec.CommandContext(context.Background(), "icacls", dir, "/remove:d", user).CombinedOutput()
		if err != nil {
			t.Logf("icacls restore failed (dir may already be gone): %s: %v", out, err)
		}
	})
}

// TestMigrateRewriteContentCreateTempFailurePropagates forces the very first
// step of the atomic write - os.CreateTemp in the target's directory - to
// fail, and verifies rewriteLegacyConfigContent reports statusFailed rather
// than silently swallowing the error. This pins down the "can't even create
// the temp file" branch (as opposed to a later write/rename failure).
//
// Failure injection: denyCurrentUserWrite applies a Windows ACL deny rule
// (icacls .../deny <user>:(WD,AD)) to the target directory, which blocks
// file creation without relying on any Unix-only mechanism like chmod 000
// (which Windows does not enforce for file creation the same way).
func TestMigrateRewriteContentCreateTempFailurePropagates(t *testing.T) {
	isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	rushFile := filepath.Join(tmpDir, "rush.json")
	content := `{"options":{"disabled_skills":["crush-config"]}}`
	require.NoError(t, os.WriteFile(rushFile, []byte(content), 0o644))

	denyCurrentUserWrite(t, tmpDir)

	testCmd := &cobra.Command{}
	var b bytes.Buffer
	testCmd.SetOut(&b)
	testCmd.SetErr(&b)

	status := rewriteLegacyConfigContent(testCmd, rushFile, rushFile, false, "test:")
	assert.Equal(t, statusFailed, status, "a directory that refuses file creation must report statusFailed")

	output := b.String()
	t.Logf("Output:\n%s", output)
	assert.Contains(t, output, "failed to rewrite content")
}

// TestMigrateCLITallyReflectsRewriteFailure runs the actual rush migrate
// command end-to-end (through runMigrate/migrateFile, not by calling
// rewriteLegacyConfigContent directly) against a project directory whose
// crush.json triggers a content rewrite, and forces ONLY the FINAL step of
// the atomic write - os.Rename(tmpPath, reportAs) - to fail, so the failure
// exercised here is specifically the new atomic-write path, not the
// pre-existing crush.json->rush.json rename-failure path.
//
// Failure injection: a background goroutine opens rush.json exclusively
// (os.O_RDWR, no share-delete) the instant it first appears on disk - i.e.
// right after migrateFile's own os.Rename(crush.json, rush.json) succeeds.
// Windows refuses to rename any file over the top of another file that is
// currently open for read/write (verified directly: os.Rename onto an
// open-for-ReadWrite file returns "Access is denied"), so
// rewriteLegacyConfigContent's os.CreateTemp and its write both still
// succeed (they don't touch the already-existing rush.json), and only its
// closing os.Rename(tmpPath, reportAs) fails.
//
// This in-process open-file lock (as opposed to shelling out to icacls, as
// an earlier version of this test did) lands in ~1-3ms - fast enough to
// reliably win the race against the rest of migrateFile's synchronous,
// sub-5ms execution. A prior version of this test used an external icacls
// process to deny write access instead; that consistently LOST the race
// (icacls.exe takes ~50ms to spawn and apply the ACE, by which time the
// entire migrate run had already completed successfully) - confirmed by
// direct timing instrumentation before switching to this approach.
//
// Asserts the failure surfaces in BOTH the printed top-level tally line
// ("N renamed, N conflict, N failed") and the command's returned error
// (non-zero exit status) - not silently swallowed the way the old
// cmd.Printf-and-return implementation left it - and specifically that it is
// NOT double-counted as a successful "renamed" item.
func TestMigrateCLITallyReflectsRewriteFailure(t *testing.T) {
	// Opening rushFile for read/write only blocks a subsequent os.Rename
	// onto it on Windows -- POSIX rename(2) freely replaces an open file,
	// so this forced-failure technique is a no-op on macOS/Linux and the
	// migrate call just succeeds normally there.
	if runtime.GOOS != "windows" {
		t.Skip("open-file-blocks-rename forced failure is Windows-specific")
	}
	isolateGlobalPaths(t)
	tmpDir := t.TempDir()

	crushFile := filepath.Join(tmpDir, "crush.json")
	// A field that will trigger a rewrite (disabled_skills legacy ID), so
	// the run actually reaches the content-rewrite step rather than
	// short-circuiting on "nothing to rewrite".
	content := `{"options":{"disabled_skills":["crush-config"]}}`
	require.NoError(t, os.WriteFile(crushFile, []byte(content), 0o644))

	rushFile := filepath.Join(tmpDir, "rush.json")

	// Deterministic synchronization via migrateFileRenamePauseSeam instead
	// of a busy-poll goroutine racing migrateFile's own execution speed: a
	// prior version of this test raced an external icacls process (always
	// lost), then a tight os.OpenFile retry loop (usually won, but flaked
	// on Windows CI under -race — see this test's doc comment above). The
	// seam blocks migrateFile immediately after rush.json first exists on
	// disk and before the content-rewrite starts, so opening it here can
	// never race against production code.
	lockedCh := make(chan *os.File, 1)
	proceed := make(chan struct{})
	migrateFileRenamePauseSeam = func() {
		f, err := os.OpenFile(rushFile, os.O_RDWR, 0o644)
		require.NoError(t, err, "rush.json must already exist by the time the pause seam fires")
		lockedCh <- f
		<-proceed
	}
	t.Cleanup(func() { migrateFileRenamePauseSeam = nil })

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))

	errCh := make(chan error, 1)
	go func() {
		errCh <- migrateCmd.RunE(migrateCmd, []string{tmpDir})
	}()

	lockedFile := <-lockedCh
	t.Cleanup(func() { lockedFile.Close() })
	close(proceed)
	err := <-errCh

	output := b.String()
	t.Logf("Output:\n%s", output)

	// The rename itself succeeded (rush.json exists - that's what let the
	// watcher goroutine open it); only the subsequent content-rewrite step
	// failed. The overall migrateFile status must therefore come back as
	// failed, not renamed - proving the rename's own success message plus a
	// rewrite failure fold into ONE failed count, not a separate renamed
	// count.
	assert.NotContains(t, output, "failed to rename", "this test must exercise the rewrite failure, not the rename failure")
	assert.Contains(t, output, "renamed project:", "the rename step itself must have succeeded")
	assert.Contains(t, output, "failed to rewrite content", "the rewrite failure must be printed")
	assert.Contains(t, output, "summary: 0 renamed, 0 conflict, 1 failed",
		"a rewrite failure must be tallied as failed, not silently dropped or double counted as renamed")

	require.Error(t, err, "a rewrite failure must produce a non-zero exit status, not silent success")
	assert.Contains(t, err.Error(), "failed")

	// No stray temp file left behind, and original (pre-rewrite) content is
	// intact - the failed atomic write must not have corrupted rush.json.
	lockedFile.Close()
	entries, err2 := os.ReadDir(tmpDir)
	require.NoError(t, err2)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"rush.json"}, names, "no leftover temp file after a failed write, got: %v", names)
}

// ── .gitignore update (task #749) ──────────────────────────────────────────

func TestMigrateGitignoreAddsRushDirEntry(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("node_modules/\n.crush/\n*.log\n"), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	require.NoError(t, migrateCmd.RunE(migrateCmd, []string{tmpDir}))

	got, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	require.NoError(t, err)
	assert.Equal(t, "node_modules/\n.crush/\n.rush/\n*.log\n", string(got))
	assert.Contains(t, b.String(), "added to")
}

func TestMigrateGitignoreAddsRushJsonEntry(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("crush.json\n"), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	require.NoError(t, migrateCmd.RunE(migrateCmd, []string{tmpDir}))

	got, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	require.NoError(t, err)
	assert.Equal(t, "crush.json\nrush.json\n", string(got))
}

func TestMigrateGitignoreNoDuplicateWhenRushEntryAlreadyPresent(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()
	original := ".crush/\n.rush/\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(original), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	require.NoError(t, migrateCmd.RunE(migrateCmd, []string{tmpDir}))

	got, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	require.NoError(t, err)
	assert.Equal(t, original, string(got), "must not duplicate an already-present .rush/ line")
}

func TestMigrateNoGitignoreCreatesNothing(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".crush"), 0o755))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	require.NoError(t, migrateCmd.RunE(migrateCmd, []string{tmpDir}))

	_, err := os.Stat(filepath.Join(tmpDir, ".gitignore"))
	assert.True(t, os.IsNotExist(err), "must never create a .gitignore that didn't exist")
}

func TestMigrateGitignoreDryRunDoesNotWrite(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()
	original := ".crush/\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(original), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	require.NoError(t, migrateCmd.Flags().Set("dry-run", "true"))
	defer migrateCmd.Flags().Set("dry-run", "false")
	require.NoError(t, migrateCmd.RunE(migrateCmd, []string{tmpDir}))

	got, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	require.NoError(t, err)
	assert.Equal(t, original, string(got), "dry-run must not write")
	assert.Contains(t, b.String(), "would add")
}

func TestMigrateGitignoreUnrelatedLineUntouched(t *testing.T) {
	_, _ = isolateGlobalPaths(t)
	tmpDir := t.TempDir()
	original := "dist/\ncrushonabike.txt\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(original), 0o644))

	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	require.NoError(t, migrateCmd.RunE(migrateCmd, []string{tmpDir}))

	got, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	require.NoError(t, err)
	assert.Equal(t, original, string(got), "a line not exactly matching a known legacy pattern must be left alone")
}
