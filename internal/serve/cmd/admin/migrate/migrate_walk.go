package migrate

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/fs/fsext"
	"github.com/spf13/cobra"
)

// migrateWalkTree recursively walks the tree rooted at root (excluding the
// root itself, which the caller migrates up front via migrateRootLocation,
// since WalkDir's callback here skips the root — renaming .crush -> .rush
// for the root mid-walk would otherwise interact badly with WalkDir's
// traversal of that same path) and migrates every legacy artifact it
// encounters: .crush directories (never descended into), loose context and
// ignore files plus .gitignore updates in every visited directory, and
// crush.json files. Returns the renamed/refused/failed counts to fold into
// the caller's running totals, whether anything was found, and the first
// walk error; per-entry errors are reported via cmd.Printf and counted
// rather than aborting the walk.
func migrateWalkTree(cmd *cobra.Command, root string, dryRun bool) (renamed, refused, failed int, foundAny bool, err error) {
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Record walk error but continue where possible.
			cmd.Printf("walk error: %v\n", err)
			failed++
			return nil
		}

		// Skip the root itself - already migrated above.
		if path == root {
			return nil
		}
		if !dryRun && d.IsDir() {
			if err := config.SettingsWriteAllowed(path); err != nil {
				return err
			}
		}

		base := filepath.Base(path)

		// Check for .crush directory BEFORE exclusion rules.
		// This ensures a gitignored .crush is still migrated.
		if d.IsDir() && base == ".crush" {
			dirStatus, innerStatus, newDir := migrateDir(cmd, path, dryRun, "project:")
			switch dirStatus {
			case statusRenamed:
				renamed++
				foundAny = true
			case statusRefused:
				refused++
				foundAny = true
			case statusFailed:
				failed++
				foundAny = true
			}
			switch innerStatus {
			case statusRenamed:
				renamed++
				foundAny = true
			case statusRefused:
				refused++
				foundAny = true
			case statusFailed:
				failed++
				foundAny = true
			}
			if dirStatus == statusRenamed {
				artRenamed, artRefused, artFailed := migrateKnownArtifacts(cmd, path, newDir, dryRun, "project:")
				renamed += artRenamed
				refused += artRefused
				failed += artFailed
				foundAny = foundAny || artRenamed > 0 || artRefused > 0 || artFailed > 0
			}
			// Never descend into a .crush directory.
			return filepath.SkipDir
		}

		// Skip ignored directories (but NOT .crush, handled above).
		if d.IsDir() && fsext.ShouldExcludeFile(root, path) {
			return filepath.SkipDir
		}

		// Check for loose context/ignore files (CRUSH.md family,
		// .crushignore) directly inside this directory. Unlike
		// crush.json/.crush, these are visited at every directory in the
		// walk (not just the root) because .crushignore is read
		// per-directory by fsext's directory lister during recursive
		// file listing (see internal/fsext/ls.go) - a stray .crushignore
		// several levels deep would otherwise keep silently excluding
		// files after the rename. Context files (CRUSH.md family) are
		// only ever loaded from the project root in practice, but
		// renaming a stray copy elsewhere is harmless, so both are
		// handled by the same directory-scoped call rather than
		// maintaining two separate walks.
		if d.IsDir() {
			ctxRenamed, ctxRefused, ctxFailed := migrateContextAndIgnoreFiles(cmd, path, dryRun, "project:")
			renamed += ctxRenamed
			refused += ctxRefused
			failed += ctxFailed
			foundAny = foundAny || ctxRenamed > 0 || ctxRefused > 0 || ctxFailed > 0

			gitignoreAdded := updateGitignoreForMigratedProject(cmd, path, dryRun, "project:")
			foundAny = foundAny || gitignoreAdded > 0
		}

		// Check for crush.json file.
		if !d.IsDir() && base == "crush.json" {
			status, _ := migrateFile(cmd, path, dryRun, "project:")
			switch status {
			case statusRenamed:
				renamed++
				foundAny = true
			case statusRefused:
				refused++
				foundAny = true
			case statusFailed:
				failed++
				foundAny = true
			}
		}

		return nil
	})
	return renamed, refused, failed, foundAny, err
}

// formatPrefix normalizes a migration status prefix to ensure exactly one space
// between label components and the following path.
func formatPrefix(parts ...string) string {
	result := ""
	for i, part := range parts {
		trimmed := trimTrailingSpace(part)
		if i > 0 && trimmed != "" {
			result += " "
		}
		result += trimmed
	}
	return result + " "
}

// trimTrailingSpace removes any trailing spaces from a string.
func trimTrailingSpace(s string) string {
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}

// migrateDir migrates a .crush directory to .rush, including the inner config.
// Returns the status of the directory migration, the status of the inner config migration,
// and the new directory path (or empty if not migrated).
func migrateDir(cmd *cobra.Command, oldDir string, dryRun bool, prefix string) (migrateStatus, migrateStatus, string) {
	// Check if legacy directory exists.
	if _, err := os.Stat(oldDir); os.IsNotExist(err) {
		return statusNone, statusNone, ""
	}

	// Build the new directory path.
	parent := filepath.Dir(oldDir)
	newDir := filepath.Join(parent, ".rush")

	// Check for conflict: new directory already exists.
	if _, err := os.Stat(newDir); err == nil {
		cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", formatPrefix(prefix), oldDir, newDir)
		return statusRefused, statusNone, ""
	}

	// Perform or report the directory rename.
	var dirStatus migrateStatus
	if dryRun {
		cmd.Printf("would rename %s%s  ->  %s\n", formatPrefix(prefix), oldDir, newDir)
		dirStatus = statusRenamed
	} else {
		if err := os.Rename(oldDir, newDir); err != nil {
			cmd.Printf("failed to rename %s%s  ->  %s: %v\n", formatPrefix(prefix), oldDir, newDir, err)
			return statusFailed, statusNone, ""
		}
		cmd.Printf("renamed %s%s  ->  %s\n", formatPrefix(prefix), oldDir, newDir)
		dirStatus = statusRenamed
	}

	// Migrate the inner workspace config (crush.json → rush.json).
	// This is a separate item for output and counters.
	// After the directory is renamed, check for the inner config at the new path.
	innerConfigPath := filepath.Join(newDir, "crush.json")
	newInnerConfigPath := filepath.Join(newDir, "rush.json")
	// Dry-run only names the target; the real-run path goes through
	// migrateFile, which derives the produced spelling from the same gate.
	if config.KtavAutoMigrateEnabled() {
		newInnerConfigPath = migrateKtavSibling(newInnerConfigPath)
	}
	configPrefix := formatPrefix(prefix, "(config inside migrated directory)")

	// Check if the legacy inner config exists (at new path after rename, or old path in dry-run).
	var legacyInnerConfig string
	if dryRun {
		legacyInnerConfig = filepath.Join(oldDir, "crush.json")
	} else {
		legacyInnerConfig = innerConfigPath
	}

	if _, err := os.Stat(legacyInnerConfig); os.IsNotExist(err) {
		// Inner config doesn't exist, only report the directory migration.
		return dirStatus, statusNone, newDir
	}

	// In dry-run mode, report the migration with the correct target path.
	if dryRun {
		// Check for conflict with either spelling of the target file in the
		// new directory.
		if existing := migrateExistingInnerConfig(newDir); existing != "" {
			cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", configPrefix, legacyInnerConfig, existing)
			return dirStatus, statusRefused, newDir
		}
		cmd.Printf("would rename %s%s  ->  %s\n", configPrefix, legacyInnerConfig, newInnerConfigPath)
		return dirStatus, statusRenamed, newDir
	}

	// In real migration mode, the directory has already been renamed.
	// Migrate the inner config file from its new location.
	innerStatus, _ := migrateFile(cmd, innerConfigPath, dryRun, configPrefix)
	return dirStatus, innerStatus, newDir
}

// migrateFile migrates a single crush.json file to rush.json.
// Returns the status and the new file path (or empty if not migrated).
func migrateFile(cmd *cobra.Command, oldFile string, dryRun bool, prefix string) (migrateStatus, string) {
	// Check if legacy file exists.
	if _, err := os.Stat(oldFile); os.IsNotExist(err) {
		return statusNone, ""
	}

	// Build the new file path. The produced spelling follows the same gate
	// config's writeTargetSlot applies to created config files: rush.ktav
	// when newly created config files default to the Ktav format, rush.json
	// otherwise. crush.json itself is only ever an INPUT — it is never
	// written back under its legacy name.
	parent := filepath.Dir(oldFile)
	base := filepath.Base(oldFile)
	var newFile string
	if base == "crush.json" {
		newFile = filepath.Join(parent, "rush.json")
		if config.KtavAutoMigrateEnabled() {
			newFile = migrateKtavSibling(newFile)
		}
	} else {
		// Unexpected file name - this shouldn't happen in normal use.
		return statusFailed, ""
	}

	// Check for conflict: EITHER spelling of the target slot already
	// exists. The conflict message names the file that is actually there,
	// so the user is told the true occupied path rather than the spelling
	// this run would have produced.
	if existing := migrateExistingInnerConfig(parent); existing != "" {
		cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", formatPrefix(prefix), oldFile, existing)
		return statusRefused, ""
	}

	// Perform or report the file rename.
	if dryRun {
		cmd.Printf("would rename %s%s  ->  %s\n", formatPrefix(prefix), oldFile, newFile)
		rewriteLegacyConfigContent(cmd, oldFile, newFile, dryRun, prefix)
		return statusRenamed, newFile
	}

	// Gate-on real run: write the converted Ktav document directly to the
	// newFile spelling (rush.ktav) and only then remove the source, so a
	// conversion or write failure leaves the original crush.json in place.
	// Gate off keeps the rename+rewrite flow below.
	if config.KtavAutoMigrateEnabled() {
		return migrateToKtavTarget(cmd, oldFile, newFile, prefix), newFile
	}

	if err := os.Rename(oldFile, newFile); err != nil {
		cmd.Printf("failed to rename %s%s  ->  %s: %v\n", formatPrefix(prefix), oldFile, newFile, err)
		return statusFailed, ""
	}

	cmd.Printf("renamed %s%s  ->  %s\n", formatPrefix(prefix), oldFile, newFile)

	if migrateFileRenamePauseSeam != nil {
		migrateFileRenamePauseSeam()
	}

	// The rename itself succeeded (the primary goal), but a failure while
	// rewriting legacy-named values inside the just-renamed content must
	// still surface as an overall failure - by this point the original
	// crush.json is already gone (renamed away above), so a rewrite failure
	// leaves rush.json as the only copy in an unknown state and the run
	// must not silently report success. See rewriteLegacyConfigContent's
	// own doc comment for what "failure" means here (I/O error on the
	// atomic write, not "nothing to rewrite").
	if rewriteStatus := rewriteLegacyConfigContent(cmd, newFile, newFile, dryRun, prefix); rewriteStatus == statusFailed {
		return statusFailed, newFile
	}
	return statusRenamed, newFile
}

// legacySkillRenames maps builtin skill IDs that were renamed as part of the
// crush->rush project to their current names. Used to fix up disabled_skills
// entries that still reference the old ID after a crush.json -> rush.json
// migration (see internal/skills/builtin for the authoritative current list
// of builtin skill directory names, and internal/skills/skills.go's Filter,
// which does a literal string comparison against skill names - a stale
// legacy ID silently stops matching anything, silently re-enabling a skill
// the user meant to keep disabled).
var legacySkillRenames = map[string]string{
	"crush-config": "rush-config",
	"crush-hooks":  "rush-hooks",
}

// legacyContextFileName is the pre-rename context-file basename that
// global_context_paths entries may still reference.
const legacyContextFileName = "CRUSH.md"

// currentContextFileName is legacyContextFileName's Rush equivalent.
const currentContextFileName = "RUSH.md"

// rewriteLegacyConfigContent reads a just-migrated rush.json (project or
// global), rewrites known legacy-named values in a small set of fields
// (disabled_skills, skills_paths, global_context_paths, data_directory),
// and writes the result back if anything changed. It is intentionally NOT a
// blind find-and-replace across the file: only these specific fields are
// touched, so a user's own data that happens to contain the substring
// "crush" for unrelated reasons (e.g. a hook command string, or a project
// description) is left alone.
//
// This function is JSON-only: it reads and writes JSON bytes and never
// converts to (or inspects) the Ktav format. Gate-on real runs no longer
// pass through here at all - they go through migrateToKtavTarget, which
// applies the same applyLegacyRewrites core before converting. This
// function remains for the gate-off real run and every dry-run preview
// (where it only prints "would rewrite" notes, never writes or converts,
// even when reportAs spells .ktav).
//
// readFrom is the path to read current content from: in a real run this is
// the already-renamed newFile; in dry-run, nothing has moved yet, so the
// caller passes the still-in-place oldFile instead (same convention
// migrateDir/migrateKnownArtifacts use elsewhere in this file). reportAs is
// always the newFile path, used both for log messages and as the write
// target.
//
// Returns statusFailed only for an actual I/O error while writing the
// rewritten content back - by that point the caller's rename has already
// happened and there is no fallback copy left, so the caller folds this
// into its own overall status rather than reporting success. Errors reading
// or parsing readFrom, or there being nothing to rewrite, are reported as
// statusNone: non-fatal, since the file has already been renamed (the
// primary goal) and there is nothing this function could have written
// anyway.
func rewriteLegacyConfigContent(cmd *cobra.Command, readFrom, reportAs string, dryRun bool, prefix string) migrateStatus {
	raw, err := os.ReadFile(readFrom)
	if err != nil {
		// Nothing to rewrite if we can't read it; migrateFile already
		// reported the rename itself.
		return statusNone
	}

	newRaw, notes, changed := applyLegacyRewrites(raw)
	if !changed {
		return statusNone
	}

	contentPrefix := formatPrefix(prefix, "(content inside migrated config)")

	if dryRun {
		for _, note := range notes {
			cmd.Printf("would rewrite %s%s: %s\n", contentPrefix, reportAs, note)
		}
		return statusNone
	}

	// Preserve the target's existing permission bits where practical (this
	// file's own writes elsewhere - e.g. in tests seeding fixtures - use
	// 0o644, so that is the fallback when the target can't be stat'd for
	// some reason).
	perm := os.FileMode(0o644)
	if info, statErr := os.Stat(reportAs); statErr == nil {
		perm = info.Mode().Perm()
	}

	// Write atomically: a plain os.WriteFile over reportAs would leave a
	// truncated/corrupt rush.json if the process dies mid-write, and by this
	// point the original crush.json has already been renamed away (no
	// fallback copy to recover from). Write to a temp file in the SAME
	// DIRECTORY as the target first (so the final os.Rename stays on one
	// filesystem - cross-filesystem renames aren't atomic), then rename it
	// over the target.
	dir := filepath.Dir(reportAs)
	tmpFile, err := os.CreateTemp(dir, filepath.Base(reportAs)+".tmp-*")
	if err != nil {
		cmd.Printf("failed to rewrite content %s%s: %v\n", contentPrefix, reportAs, err)
		return statusFailed
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(append(newRaw, '\n')); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		cmd.Printf("failed to rewrite content %s%s: %v\n", contentPrefix, reportAs, err)
		return statusFailed
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		cmd.Printf("failed to rewrite content %s%s: %v\n", contentPrefix, reportAs, err)
		return statusFailed
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		cmd.Printf("failed to rewrite content %s%s: %v\n", contentPrefix, reportAs, err)
		return statusFailed
	}

	if err := os.Rename(tmpPath, reportAs); err != nil {
		os.Remove(tmpPath)
		cmd.Printf("failed to rewrite content %s%s: %v\n", contentPrefix, reportAs, err)
		return statusFailed
	}

	for _, note := range notes {
		cmd.Printf("rewrote %s%s: %s\n", contentPrefix, reportAs, note)
	}
	return statusRenamed
}

// rewriteLegacyConfigPath rewrites a single path-like config value:
//   - a ".crush" path segment becomes ".rush" (segment-boundary aware, using
//     filepath.SplitList/separators rather than a naive string replace, so
//     "/foo/notcrushbar/skills" is left untouched while "/foo/.crush/skills"
//     becomes "/foo/.rush/skills")
//   - a bare "crush" segment immediately following a ".config" segment
//     becomes "rush" - the XDG-style config-home convention
//     ("~/.config/crush/..." -> "~/.config/rush/..."), matched narrowly on
//     that specific ".config/crush" adjacency so an unrelated path like
//     "/data/crush/skills" (a "crush" directory with no ".config" parent)
//     is left untouched
//   - a "CRUSH.md" basename becomes "RUSH.md"
//
// Returns the input unchanged if none of the above apply.
func rewriteLegacyConfigPath(p string) string {
	if p == "" {
		return p
	}

	result := p

	// Rewrite CRUSH.md as a path segment (basename or any component),
	// matched on segment boundaries the same way as .crush below.
	result = rewritePathSegments(result, legacyContextFileName, currentContextFileName)

	// Rewrite a bare ".crush" path segment to ".rush".
	result = rewritePathSegments(result, ".crush", ".rush")

	// Rewrite the XDG-style "<...>/.config/crush/..." convention, where
	// "crush" (no leading dot) is its own segment right after ".config".
	result = rewriteSegmentAfter(result, ".config", "crush", "rush")

	return result
}

// splitPathSegments splits p into path segments on both / and \ (so it
// behaves correctly for paths written with either separator convention -
// config files are portable text, and a path saved on one OS may be
// read/migrated on another), returning the segments and the separator byte
// that followed each one (len(seps) == len(segments)-1).
func splitPathSegments(p string) (segments []string, seps []byte) {
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' || p[i] == '\\' {
			segments = append(segments, p[start:i])
			seps = append(seps, p[i])
			start = i + 1
		}
	}
	segments = append(segments, p[start:])
	return segments, seps
}

// joinPathSegments rejoins segments using the separators splitPathSegments
// returned, reconstructing the original path exactly aside from whatever
// segment values the caller mutated in place.
func joinPathSegments(segments []string, seps []byte) string {
	var b strings.Builder
	for i, seg := range segments {
		b.WriteString(seg)
		if i < len(seps) {
			b.WriteByte(seps[i])
		}
	}
	return b.String()
}

// rewritePathSegments replaces path segments that are exactly oldSeg with
// newSeg. Segments are compared verbatim (case-sensitive), matching the
// exact legacy names this migration targets (".crush", "CRUSH.md").
func rewritePathSegments(p, oldSeg, newSeg string) string {
	if !strings.Contains(p, oldSeg) {
		return p
	}

	segments, seps := splitPathSegments(p)

	changed := false
	for i, seg := range segments {
		if seg == oldSeg {
			segments[i] = newSeg
			changed = true
		}
	}
	if !changed {
		return p
	}

	return joinPathSegments(segments, seps)
}

// rewriteSegmentAfter replaces a segment exactly equal to oldSeg with
// newSeg, but only when the immediately preceding segment is exactly
// afterSeg. Used for the narrow "<...>/.config/crush/..." XDG convention:
// a bare "crush" segment is only rewritten when it directly follows a
// ".config" segment, so an unrelated "crush" directory elsewhere in a path
// (no ".config" parent) is left untouched.
func rewriteSegmentAfter(p, afterSeg, oldSeg, newSeg string) string {
	if !strings.Contains(p, oldSeg) {
		return p
	}

	segments, seps := splitPathSegments(p)

	changed := false
	for i := 1; i < len(segments); i++ {
		if segments[i] == oldSeg && segments[i-1] == afterSeg {
			segments[i] = newSeg
			changed = true
		}
	}
	if !changed {
		return p
	}

	return joinPathSegments(segments, seps)
}
