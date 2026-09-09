package cmd

// Renaming the non-config artifacts a migrated directory carries: known per-file renames, the context and ignore files, .gitignore patterns, and the global-location merge that has to reconcile a legacy tree with an existing current one. Split out of migrate.go when the 1000-line file limit landed.

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// knownArtifactFileRename describes a single known non-config artifact
// whose basename changes from the legacy crush-style name to the rush-style
// name. dir is the relative subdirectory the file lives in ("" for the top
// level of the migrated directory, "logs" for files nested one level down).
type knownArtifactFileRename struct {
	dir     string // relative subdirectory, "" for top-level
	oldName string
	newName string
}

// knownArtifactFileRenames is the single source of truth for known
// non-config artifact basename renames applied inside a migrated global or
// project directory: crush.db -> rush.db and logs/crush.log ->
// logs/rush.log. Both migrateKnownArtifacts (whole-directory-rename path)
// and mergeRemainingDirEntries (per-item merge path, when the target
// directory already existed) share this table so there is exactly one
// place that knows the crush->rush artifact name mapping.
func knownArtifactFileRenames() []knownArtifactFileRename {
	return []knownArtifactFileRename{
		{dir: "", oldName: "crush.db", newName: "rush.db"},
		{dir: "logs", oldName: "crush.log", newName: "rush.log"},
	}
}

// migrateKnownArtifacts renames known non-config artifact files inside a
// directory that has just been (or, in dry-run, would be) renamed from
// .crush-style to .rush-style: crush.db -> rush.db and logs/crush.log ->
// logs/rush.log. These files only change name, not location — the "logs"
// subdirectory itself is never renamed, only the file inside it, and only
// if "logs" exists at all (a project-level .crush that never emitted logs
// won't have it).
//
// oldDir is the pre-rename directory path (e.g. ".../.crush" or the legacy
// global dir); newDir is the post-rename path (e.g. ".../.rush"). In a real
// run the directory rename has already happened by the time this is called,
// so the artifacts are looked up under newDir. In dry-run, nothing has
// actually moved yet, so source files are looked up under oldDir instead —
// same convention migrateDir uses for its inner-config dry-run check —
// while conflict/target messages still report the newDir path names.
//
// Same conflict-refusal discipline as every other rename in this file:
// never clobber an existing rush-named target, refuse and report instead.
// Returns renamed/refused/failed counts to fold into the caller's totals.
func migrateKnownArtifacts(cmd *cobra.Command, oldDir, newDir string, dryRun bool, prefix string) (renamed, refused, failed int) {
	tally := func(status migrateStatus) {
		switch status {
		case statusRenamed:
			renamed++
		case statusRefused:
			refused++
		case statusFailed:
			failed++
		}
	}

	sourceDir := newDir
	if dryRun {
		sourceDir = oldDir
	}

	artifactPrefix := formatPrefix(prefix, "(artifact inside migrated directory)")

	for _, art := range knownArtifactFileRenames() {
		sourceSubDir := sourceDir
		newSubDir := newDir
		if art.dir != "" {
			sourceSubDir = filepath.Join(sourceDir, art.dir)
			newSubDir = filepath.Join(newDir, art.dir)
			if _, err := os.Stat(sourceSubDir); err != nil {
				continue
			}
		}

		status, _ := migrateNamedFile(cmd, filepath.Join(sourceSubDir, art.oldName), filepath.Join(newSubDir, art.newName), dryRun, artifactPrefix)
		tally(status)
	}

	return renamed, refused, failed
}

// migrateNamedFile renames oldFile to newFile directly (unlike migrateFile,
// which derives newFile from oldFile by assuming the "crush.json" ->
// "rush.json" basename swap). Used for known artifact files whose old/new
// basenames differ in ways migrateFile doesn't handle (crush.db -> rush.db,
// crush.log -> rush.log). Shares the same not-exists/conflict/dry-run
// semantics as migrateFile.
func migrateNamedFile(cmd *cobra.Command, oldFile, newFile string, dryRun bool, prefix string) (migrateStatus, string) {
	// Check if the source file exists.
	if _, err := os.Stat(oldFile); os.IsNotExist(err) {
		return statusNone, ""
	}

	// Check for conflict: new file already exists.
	if _, err := os.Stat(newFile); err == nil {
		cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", formatPrefix(prefix), oldFile, newFile)
		return statusRefused, ""
	}

	// Perform or report the file rename.
	if dryRun {
		cmd.Printf("would rename %s%s  ->  %s\n", formatPrefix(prefix), oldFile, newFile)
		return statusRenamed, newFile
	}

	if err := os.Rename(oldFile, newFile); err != nil {
		cmd.Printf("failed to rename %s%s  ->  %s: %v\n", formatPrefix(prefix), oldFile, newFile, err)
		return statusFailed, ""
	}

	cmd.Printf("renamed %s%s  ->  %s\n", formatPrefix(prefix), oldFile, newFile)
	return statusRenamed, newFile
}

// legacyContextAndIgnoreFileRenames is the single source of truth for the
// loose (not inside .crush/) project-root files whose legacy crush-branded
// basename must be renamed to its rush-branded equivalent so the app keeps
// finding them after a rename:
//
//   - Context files: every crush-equivalent of a case variant that
//     internal/config/config.go's defaultContextPaths actually looks for
//     (rush.md, rush.local.md, Rush.md, Rush.local.md, RUSH.md,
//     RUSH.local.md) - NOT every case spelling anyone could imagine, only
//     the ones the config loader reads. A pre-existing CRUSH.md-family file
//     otherwise silently stops being loaded as agent context after
//     upgrading, with no warning (defaultContextPaths has no crush.md
//     entries at all).
//   - Ignore file: .crushignore -> .rushignore. internal/fsext/ls.go's
//     per-directory ignore-file reader only looks for .gitignore and
//     .rushignore; a pre-existing .crushignore silently stops excluding
//     files, so previously-excluded files quietly re-enter agent context.
//
// Both are project-scoped, not global: defaultContextPaths is resolved via
// processContextPath joined against the workspace's WorkingDir (see
// internal/agent/prompt/prompt.go), and .rushignore is read per-directory by
// fsext's directory lister during recursive file listing - neither concept
// has a global/user-home equivalent the way crush.json's global config/data
// locations do, so these renames are only wired into the project-directory
// call sites (migrateRootLocation and the --recursive WalkDir callback in
// runMigrate), not migrateGlobalLocation.
//
// This is intentionally a separate table from knownArtifactFileRenames
// rather than folded into it: artifacts live INSIDE a migrated .crush/.rush
// directory and are only processed when that directory itself is renamed,
// while these files live loose in whatever directory is being visited
// (project root, or - for ignore files - any directory during a recursive
// walk) regardless of whether a .crush directory is even present there.
func legacyContextAndIgnoreFileRenames() []knownArtifactFileRename {
	return []knownArtifactFileRename{
		{oldName: "crush.md", newName: "rush.md"},
		{oldName: "crush.local.md", newName: "rush.local.md"},
		{oldName: "Crush.md", newName: "Rush.md"},
		{oldName: "Crush.local.md", newName: "Rush.local.md"},
		{oldName: "CRUSH.md", newName: "RUSH.md"},
		{oldName: "CRUSH.local.md", newName: "RUSH.local.md"},
		{oldName: ".crushignore", newName: ".rushignore"},
	}
}

// migrateContextAndIgnoreFiles renames any legacy context/ignore files
// present directly inside dir (see legacyContextAndIgnoreFileRenames for the
// full list and rationale) to their rush-branded equivalents. Same
// conflict-refusal discipline as every other rename in this file: a target
// that already exists refuses just that one item and is reported, everything
// else still proceeds.
//
// Case-insensitive-filesystem hazard: on Windows/default-macOS, os.Stat
// matches names case-insensitively, so a naive "does the target exist"
// check using os.Stat(newPath) would false-positive whenever the legacy and
// target names differ only by case in a way the filesystem folds together
// (verified concretely on this machine: after creating "rush.md",
// os.Stat("RUSH.md") also succeeds and os.SameFile confirms it is the same
// file). None of the mappings above are pure case-only renames (each drops
// the leading "C"/"c"), so a source can never collide with its own target
// this way, but a DIFFERENT already-existing file could still collide
// case-insensitively with a target name (e.g. an existing "Rush.md" blocks
// renaming "Crush.md" -> "Rush.md" even though the exact-case spellings
// differ). To classify a Stat hit as a genuine conflict rather than a
// same-file false positive, os.SameFile is used to compare the (about to be
// vacated) source path against the Stat-matched target path: same file ->
// not a conflict, just proceed with the rename (os.Rename handles pure
// case-only renames correctly on Windows, confirmed by direct testing);
// different file -> genuine conflict, refuse and report.
func migrateContextAndIgnoreFiles(cmd *cobra.Command, dir string, dryRun bool, prefix string) (renamed, refused, failed int) {
	tally := func(status migrateStatus) {
		switch status {
		case statusRenamed:
			renamed++
		case statusRefused:
			refused++
		case statusFailed:
			failed++
		}
	}

	itemPrefix := formatPrefix(prefix, "(context/ignore file)")

	for _, rn := range legacyContextAndIgnoreFileRenames() {
		oldPath := filepath.Join(dir, rn.oldName)
		newPath := filepath.Join(dir, rn.newName)
		status, _ := migrateNamedFileCaseAware(cmd, oldPath, newPath, dryRun, itemPrefix)
		tally(status)
	}

	return renamed, refused, failed
}

// migrateNamedFileCaseAware behaves like migrateNamedFile (rename oldFile to
// newFile, same not-exists/conflict/dry-run semantics) but adds an
// os.SameFile check before reporting a conflict, so a case-insensitive
// filesystem's case-folded Stat match against a DIFFERENT legacy name that
// happens to resolve to the same underlying file as oldFile is not
// misreported as a conflict against oldFile itself. See
// migrateContextAndIgnoreFiles for the concrete scenario this guards
// against.
func migrateNamedFileCaseAware(cmd *cobra.Command, oldFile, newFile string, dryRun bool, prefix string) (migrateStatus, string) {
	oldInfo, err := os.Stat(oldFile)
	if os.IsNotExist(err) {
		return statusNone, ""
	}
	if err != nil {
		return statusNone, ""
	}

	if newInfo, err := os.Stat(newFile); err == nil {
		if !os.SameFile(oldInfo, newInfo) {
			cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", formatPrefix(prefix), oldFile, newFile)
			return statusRefused, ""
		}
		// Same underlying file (case-insensitive filesystem folded oldFile
		// and newFile to the same inode) - fall through to the rename below,
		// which os.Rename handles correctly even for a pure case-only
		// change.
	}

	if dryRun {
		cmd.Printf("would rename %s%s  ->  %s\n", formatPrefix(prefix), oldFile, newFile)
		return statusRenamed, newFile
	}

	if err := os.Rename(oldFile, newFile); err != nil {
		cmd.Printf("failed to rename %s%s  ->  %s: %v\n", formatPrefix(prefix), oldFile, newFile, err)
		return statusFailed, ""
	}

	cmd.Printf("renamed %s%s  ->  %s\n", formatPrefix(prefix), oldFile, newFile)
	return statusRenamed, newFile
}

// gitignoreLegacyPatternRenames: exact .gitignore lines this migrator can
// add a rush-named counterpart for. Excludes logs/crush.log — gitignore
// entries for nested log files vary too much to guess safely.
func gitignoreLegacyPatternRenames() []knownArtifactFileRename {
	renames := []knownArtifactFileRename{
		{oldName: ".crush", newName: ".rush"},
		{oldName: ".crush/", newName: ".rush/"},
		{oldName: "crush.json", newName: "rush.json"},
	}
	for _, art := range knownArtifactFileRenames() {
		if art.dir == "" {
			renames = append(renames, knownArtifactFileRename{oldName: art.oldName, newName: art.newName})
		}
	}
	return append(renames, legacyContextAndIgnoreFileRenames()...)
}

// updateGitignoreForMigratedProject adds a rush-named line after any
// matching legacy line in dir/.gitignore. Additive only — never rewrites,
// removes, or creates a .gitignore, never duplicates, never commits.
// Exact-line matching only, no glob parsing. Returns lines added.
func updateGitignoreForMigratedProject(cmd *cobra.Command, dir string, dryRun bool, prefix string) int {
	path := filepath.Join(dir, ".gitignore")
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0 // no .gitignore present — never create one
	}

	usesCRLF := strings.Contains(string(raw), "\r\n")
	lines := strings.Split(string(raw), "\n")

	present := make(map[string]bool, len(lines))
	for _, l := range lines {
		present[strings.TrimRight(l, "\r")] = true
	}

	renames := gitignoreLegacyPatternRenames()
	itemPrefix := formatPrefix(prefix, "(.gitignore)")

	var out []string
	var added []string
	for _, l := range lines {
		trimmed := strings.TrimRight(l, "\r")
		out = append(out, l)
		for _, rn := range renames {
			if trimmed != rn.oldName || present[rn.newName] {
				continue
			}
			newLine := rn.newName
			if usesCRLF {
				newLine += "\r"
			}
			out = append(out, newLine)
			present[rn.newName] = true
			added = append(added, rn.newName)
			break
		}
	}

	if len(added) == 0 {
		return 0
	}

	if dryRun {
		for _, a := range added {
			cmd.Printf("would add %s%s: %s\n", itemPrefix, path, a)
		}
		return len(added)
	}

	// strings.Split preserved a trailing "" element if raw ended in \n, so
	// Join reproduces the original trailing-newline state without extra logic.
	newContent := strings.Join(out, "\n")

	// Atomic write, same pattern as rewriteLegacyConfigContent.
	info, statErr := os.Stat(path)
	perm := os.FileMode(0o644)
	if statErr == nil {
		perm = info.Mode().Perm()
	}
	tmpFile, err := os.CreateTemp(dir, ".gitignore.tmp-*")
	if err != nil {
		cmd.Printf("failed to update %s%s: %v\n", itemPrefix, path, err)
		return 0
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.WriteString(newContent); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		cmd.Printf("failed to update %s%s: %v\n", itemPrefix, path, err)
		return 0
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		cmd.Printf("failed to update %s%s: %v\n", itemPrefix, path, err)
		return 0
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		cmd.Printf("failed to update %s%s: %v\n", itemPrefix, path, err)
		return 0
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		cmd.Printf("failed to update %s%s: %v\n", itemPrefix, path, err)
		return 0
	}

	for _, a := range added {
		cmd.Printf("added to %s%s: %s\n", itemPrefix, path, a)
	}
	return len(added)
}

// migrateGlobalLocation migrates a global config or data location.
// Handles two cases:
// 1. Whole directory rename (when target directory doesn't exist).
// 2. File-level rename (when both directories exist or are the same).
//
// Returns the status of the directory/file migration, the status of the
// inner config migration, and the renamed/refused/failed counts of extra
// items migrated alongside the primary rename: known artifact files
// (crush.db, logs/crush.log) for a whole directory rename (case 1), or
// every remaining legacy-directory entry moved into the target directory
// for a file-level rename that leaves the legacy directory non-empty
// (case 2 — see the "merge remaining entries" step below).
//
// When renaming just the file and the legacy directory becomes empty, it is
// removed. If the directory still has other contents (e.g., auth.json, skills),
// a notice is printed and the operation is counted as refused (unfinished
// manual work remains).
func migrateGlobalLocation(cmd *cobra.Command, legacyPath, currentPath string, dryRun bool, prefix string) (dirStatus, innerStatus migrateStatus, artifactRenamed, artifactRefused, artifactFailed int) {
	legacyDir := filepath.Dir(legacyPath)
	rushDir := filepath.Dir(currentPath)

	// Check if legacy directory exists.
	if _, err := os.Stat(legacyDir); os.IsNotExist(err) {
		return statusNone, statusNone, 0, 0, 0
	}

	// If legacy and rush directories are the same (env-var-isolated case),
	// reduce to file-level migration.
	if legacyDir == rushDir {
		status, _ := migrateFile(cmd, legacyPath, dryRun, prefix)
		return status, statusNone, 0, 0, 0
	}

	// Case 1: target directory doesn't exist - rename whole directory.
	if _, err := os.Stat(rushDir); os.IsNotExist(err) {
		if dryRun {
			cmd.Printf("would rename %s%s  ->  %s\n", formatPrefix(prefix), legacyDir, rushDir)
			// Check for inner config in dry-run mode.
			innerConfigPath := filepath.Join(legacyDir, "crush.json")
			newInnerConfigPath := filepath.Join(rushDir, "rush.json")
			configPrefix := formatPrefix(prefix, "(config inside migrated directory)")
			artRenamed, artRefused, artFailed := migrateKnownArtifacts(cmd, legacyDir, rushDir, dryRun, prefix)
			if _, err := os.Stat(innerConfigPath); !os.IsNotExist(err) {
				if _, err := os.Stat(newInnerConfigPath); err == nil {
					cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", configPrefix, innerConfigPath, newInnerConfigPath)
					return statusRenamed, statusRefused, artRenamed, artRefused, artFailed
				}
				cmd.Printf("would rename %s%s  ->  %s\n", configPrefix, innerConfigPath, newInnerConfigPath)
				return statusRenamed, statusRenamed, artRenamed, artRefused, artFailed
			}
			return statusRenamed, statusNone, artRenamed, artRefused, artFailed
		}
		if err := os.Rename(legacyDir, rushDir); err != nil {
			cmd.Printf("failed to rename %s%s  ->  %s: %v\n", formatPrefix(prefix), legacyDir, rushDir, err)
			return statusFailed, statusNone, 0, 0, 0
		}
		cmd.Printf("renamed %s%s  ->  %s\n", formatPrefix(prefix), legacyDir, rushDir)

		// Migrate known artifact files (crush.db, logs/crush.log) alongside
		// the directory rename, same as migrateDir does for project dirs.
		artRenamed, artRefused, artFailed := migrateKnownArtifacts(cmd, legacyDir, rushDir, dryRun, prefix)

		// Migrate the inner config file.
		configPrefix := formatPrefix(prefix, "(config inside migrated directory)")
		innerStat, _ := migrateFile(cmd, filepath.Join(rushDir, "crush.json"), dryRun, configPrefix)
		return statusRenamed, innerStat, artRenamed, artRefused, artFailed
	}

	// Case 2: target directory exists - file-level migration.
	// Check if legacy file exists.
	if _, err := os.Stat(legacyPath); os.IsNotExist(err) {
		return statusNone, statusNone, 0, 0, 0
	}

	// Check if target file exists (conflict).
	if _, err := os.Stat(currentPath); err == nil {
		cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", formatPrefix(prefix), legacyPath, currentPath)
		return statusRefused, statusNone, 0, 0, 0
	}

	// Perform or report the file rename.
	if dryRun {
		cmd.Printf("would rename %s%s  ->  %s\n", formatPrefix(prefix), legacyPath, currentPath)
		return statusRenamed, statusNone, 0, 0, 0
	}

	if err := os.Rename(legacyPath, currentPath); err != nil {
		cmd.Printf("failed to rename %s%s  ->  %s: %v\n", formatPrefix(prefix), legacyPath, currentPath, err)
		return statusFailed, statusNone, 0, 0, 0
	}

	cmd.Printf("renamed %s%s  ->  %s\n", formatPrefix(prefix), legacyPath, currentPath)

	// Check if legacy directory is now empty.
	entries, err := os.ReadDir(legacyDir)
	if err != nil {
		// Failed to read directory - can't determine emptiness, leave it.
		return statusRenamed, statusNone, 0, 0, 0
	}

	if len(entries) == 0 {
		// Directory is empty - remove it.
		if err := os.Remove(legacyDir); err != nil {
			cmd.Printf("notice: failed to remove now-empty legacy directory %s: %v\n", legacyDir, err)
		}
		return statusRenamed, statusNone, 0, 0, 0
	}

	// Directory has other contents (e.g., auth.json, skills/) - the target
	// directory already existed (that's why we're in case 2 at all), so
	// these were stranded rather than moved by a whole-directory rename.
	// Attempt to move every remaining entry into the target directory,
	// same never-clobber discipline as everything else: a per-item name
	// conflict refuses just that item and reports it, everything else that
	// doesn't conflict still gets moved.
	mergeRenamed, mergeRefused, mergeFailed, refusedNames := mergeRemainingDirEntries(cmd, legacyDir, rushDir, dryRun, prefix)

	// Re-check emptiness after the merge attempt - if everything moved (or
	// there was nothing left to conflict on), remove the now-empty legacy
	// directory, same as the fast path above.
	remaining, err := os.ReadDir(legacyDir)
	if err == nil && len(remaining) == 0 {
		if !dryRun {
			if err := os.Remove(legacyDir); err != nil {
				cmd.Printf("notice: failed to remove now-empty legacy directory %s: %v\n", legacyDir, err)
			}
		}
	} else if len(refusedNames) > 0 {
		cmd.Printf("notice: legacy directory %s still contains %d item(s) that were NOT merged due to name conflicts with the target directory: %s. Resolve manually (merge by hand, then delete the old path).\n", legacyDir, len(refusedNames), strings.Join(refusedNames, ", "))
	}

	// Count as refused overall so the exit code reflects unfinished work
	// whenever at least one item could not be merged.
	if mergeRefused > 0 || mergeFailed > 0 {
		return statusRenamed, statusNone, mergeRenamed, mergeRefused, mergeFailed
	}
	return statusRenamed, statusNone, mergeRenamed, 0, mergeFailed
}

// mergeRemainingDirEntries moves every entry still present in legacyDir into
// targetDir, used when a legacy global directory (e.g. ~/.config/crush) has
// leftover files/subdirectories (skills/, auth.json, crush.db,
// logs/crush.log, etc.) after its crush.json was already migrated via a
// file-level rename in migrateGlobalLocation's case 2 (the target directory
// already existed, so there was no single whole-directory rename to carry
// these along).
//
// Known artifacts (crush.db, logs/crush.log) are moved under their mapped
// rush-style name via knownArtifactFileRenames, the same table
// migrateKnownArtifacts uses for the whole-directory-rename path — the app
// looks for rush.db and logs/rush.log, so landing them under their old
// crush-style names here would make them invisible to the app even though
// the migration reported success. Everything else is moved unchanged.
//
// logs/ is handled as a per-file merge into (possibly newly created)
// targetDir/logs rather than a whole-directory os.Rename: the target
// directory may already have its own logs/ with unrelated content (or none
// yet), so only the specific crush.log -> rush.log entry inside it is
// renamed/merged, and any other files already in a stranded logs/ are
// merged in under their own names via the same conflict-refusing loop.
//
// Same conflict-refusal discipline as every rename in this file: an entry
// is moved with os.Rename unless the target directory already has an entry
// with that (possibly mapped) name, in which case that one entry is refused
// and reported — by its ORIGINAL name, so the user can find the file being
// discussed — while every other, non-conflicting entry still proceeds.
//
// Returns renamed/refused/failed counts plus the base names of any refused
// entries, so the caller can name them explicitly in its own notice rather
// than falling back to a generic "other files" message.
func mergeRemainingDirEntries(cmd *cobra.Command, legacyDir, targetDir string, dryRun bool, prefix string) (renamed, refused, failed int, refusedNames []string) {
	entries, err := os.ReadDir(legacyDir)
	if err != nil {
		return 0, 0, 0, nil
	}

	mergePrefix := formatPrefix(prefix, "(remaining item in migrated directory)")

	// Map of top-level entry name -> mapped target name for known artifacts
	// that live directly inside legacyDir (currently just crush.db).
	topLevelRenames := map[string]string{}
	// Whether "logs" needs special per-file merge treatment instead of a
	// plain whole-entry rename.
	logsNeedsMerge := false
	for _, art := range knownArtifactFileRenames() {
		switch art.dir {
		case "":
			topLevelRenames[art.oldName] = art.newName
		case "logs":
			logsNeedsMerge = true
		}
	}

	for _, entry := range entries {
		name := entry.Name()
		oldPath := filepath.Join(legacyDir, name)

		if name == "logs" && logsNeedsMerge && entry.IsDir() {
			logsRenamed, logsRefused, logsFailed, logsRefusedNames := mergeLogsDirEntries(cmd, oldPath, filepath.Join(targetDir, "logs"), dryRun, mergePrefix)
			renamed += logsRenamed
			refused += logsRefused
			failed += logsFailed
			for _, n := range logsRefusedNames {
				refusedNames = append(refusedNames, filepath.Join(name, n))
			}
			continue
		}

		mappedName := name
		if mapped, ok := topLevelRenames[name]; ok {
			mappedName = mapped
		}
		newPath := filepath.Join(targetDir, mappedName)

		if _, err := os.Stat(newPath); err == nil {
			cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", mergePrefix, oldPath, newPath)
			refused++
			refusedNames = append(refusedNames, name)
			continue
		}

		if dryRun {
			cmd.Printf("would rename %s%s  ->  %s\n", mergePrefix, oldPath, newPath)
			renamed++
			continue
		}

		if err := os.Rename(oldPath, newPath); err != nil {
			cmd.Printf("failed to rename %s%s  ->  %s: %v\n", mergePrefix, oldPath, newPath, err)
			failed++
			continue
		}

		cmd.Printf("renamed %s%s  ->  %s\n", mergePrefix, oldPath, newPath)
		renamed++
	}

	return renamed, refused, failed, refusedNames
}

// mergeLogsDirEntries merges every entry of a stranded legacy logs/
// directory into targetLogsDir, mapping known log artifact names (currently
// crush.log -> rush.log) the same way mergeRemainingDirEntries maps
// top-level artifacts. targetLogsDir is created if it does not exist yet
// (in a real run); in dry-run nothing is created, matching the rest of this
// file's dry-run convention.
//
// Same conflict-refusal discipline as mergeRemainingDirEntries: a name
// collision on the (possibly mapped) target refuses just that one entry,
// reported by its original name, while everything else still merges.
func mergeLogsDirEntries(cmd *cobra.Command, legacyLogsDir, targetLogsDir string, dryRun bool, prefix string) (renamed, refused, failed int, refusedNames []string) {
	entries, err := os.ReadDir(legacyLogsDir)
	if err != nil {
		return 0, 0, 0, nil
	}

	if !dryRun {
		if err := os.MkdirAll(targetLogsDir, 0o755); err != nil {
			cmd.Printf("failed to create %s%s: %v\n", prefix, targetLogsDir, err)
			return 0, 0, len(entries), nil
		}
	}

	for _, entry := range entries {
		name := entry.Name()
		oldPath := filepath.Join(legacyLogsDir, name)

		mappedName := name
		if name == "crush.log" {
			mappedName = "rush.log"
		}
		newPath := filepath.Join(targetLogsDir, mappedName)

		if _, err := os.Stat(newPath); err == nil {
			cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", prefix, oldPath, newPath)
			refused++
			refusedNames = append(refusedNames, name)
			continue
		}

		if dryRun {
			cmd.Printf("would rename %s%s  ->  %s\n", prefix, oldPath, newPath)
			renamed++
			continue
		}

		if err := os.Rename(oldPath, newPath); err != nil {
			cmd.Printf("failed to rename %s%s  ->  %s: %v\n", prefix, oldPath, newPath, err)
			failed++
			continue
		}

		cmd.Printf("renamed %s%s  ->  %s\n", prefix, oldPath, newPath)
		renamed++
	}

	// If the legacy logs dir is now empty (and not dry-run), remove it so
	// the outer caller's emptiness check on legacyDir can also succeed.
	if !dryRun {
		if remaining, err := os.ReadDir(legacyLogsDir); err == nil && len(remaining) == 0 {
			if err := os.Remove(legacyLogsDir); err != nil {
				cmd.Printf("notice: failed to remove now-empty legacy directory %s: %v\n", legacyLogsDir, err)
			}
		}
	}

	return renamed, refused, failed, refusedNames
}
