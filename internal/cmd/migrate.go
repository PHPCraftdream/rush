package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/PHPCraftdream/rush/internal/home"
	"github.com/spf13/cobra"
)

// migrateStatus represents the outcome of a migration attempt for a single item.
type migrateStatus int

const (
	statusNone    migrateStatus = iota // Item absent, nothing to do
	statusRenamed                      // Successfully renamed
	statusRefused                      // Conflict - target exists
	statusFailed                       // I/O error during migration
)

var migrateCmd = &cobra.Command{
	Use:   "migrate [root]",
	Short: "Migrate legacy Crush files (.crush/, crush.json) to Rush names (.rush/, rush.json)",
	Long: `Migrate legacy pre-rename Crush files and directories to their Rush equivalents.
This command renames .crush/ directories to .rush/ and crush.json files to rush.json,
including workspace configs inside migrated directories and global config/data locations.

By default, operates on the current working directory (or --cwd if set) and always
attempts global migrations. With --recursive, walks the entire directory tree
starting at the given root, including the root itself.

A --dry-run reports what would happen without making any changes.

Conflict rule: if the target path already exists, the item is refused with a clear
message and left untouched. Never clobbers existing Rush files or directories.

Ignored directories (.git, node_modules, vendor, .rush, etc.) are skipped during
recursive walks using gitignore/common-pattern-aware exclusion rules. However,
a .crush directory that matches ignore patterns is still migrated (the name check
happens before the ignore check).

Known artifact files inside a migrated .crush/ are also renamed to their Rush
equivalents: crush.db → rush.db and logs/crush.log → logs/rush.log. Any other
files inside are left as-is and not touched.

After a crush.json is renamed to rush.json, its content is also checked for
legacy-named values in four fields and rewritten in place: disabled_skills
entries matching a renamed builtin skill ID (e.g. crush-config → rush-config),
and .crush-referencing path segments (including the ~/.config/crush convention)
or the old CRUSH.md context-file name in skills_paths, global_context_paths,
and data_directory. No other fields or values are touched - this is not a
blind find-and-replace, so unrelated content (e.g. a hook command string that
happens to mention "crush") is left as-is.

Global locations (always processed, regardless of --recursive):
  - Config: ~XDG_CONFIG_HOME-or-~/.config/crush/crush.json → rush/rush.json
  - Data: XDG_DATA_HOME/crush/crush.json → rush/rush.json (or %LOCALAPPDATA%/crush/ on Windows)
  - System config (Unix only): /etc/crush/crush.json → /etc/rush/rush.json. This
    location is often root-owned; migrating it may require elevated privileges.`,
	Example: `
# Migrate current directory and global locations
rush migrate

# Preview changes without modifying anything
rush migrate --dry-run

# Recursively migrate a specific directory tree
rush migrate --recursive /path/to/project
  `,
	Args: cobra.MaximumNArgs(1),
	RunE: runMigrate,
}

func init() {
	migrateCmd.Flags().BoolP("recursive", "r", false, "Recursively walk the directory tree")
	migrateCmd.Flags().Bool("dry-run", false, "Report what would happen without making changes")
	rootCmd.AddCommand(migrateCmd)
}

// runMigrate implements the migrate command logic.
func runMigrate(cmd *cobra.Command, args []string) error {
	recursive, _ := cmd.Flags().GetBool("recursive")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	// Determine the root path for project migration.
	var root string
	var err error
	if len(args) > 0 {
		root = args[0]
		if !filepath.IsAbs(root) {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get current working directory: %w", err)
			}
			root = filepath.Join(cwd, root)
		}
	} else {
		root, err = ResolveCwd(cmd)
		if err != nil {
			return fmt.Errorf("failed to resolve working directory: %w", err)
		}
	}

	// Normalize the root path to ensure consistent comparison.
	root = filepath.Clean(root)

	// Initialize counters.
	renamedCount := 0
	refusedCount := 0
	failedCount := 0

	// Track whether we found anything to report.
	foundAny := false

	// Migrate project locations (current dir or recursive walk).
	if recursive {
		// Migrate the root itself using the same logic the non-recursive
		// branch uses, since WalkDir's callback skips the root (it would
		// otherwise see .crush/.rush changing mid-walk at the top level).
		rootRenamed, rootRefused, rootFailed, rootFoundAny := migrateRootLocation(cmd, root, dryRun)
		renamedCount += rootRenamed
		refusedCount += rootRefused
		failedCount += rootFailed
		foundAny = foundAny || rootFoundAny

		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				// Record walk error but continue where possible.
				cmd.Printf("walk error: %v\n", err)
				failedCount++
				return nil
			}

			// Skip the root itself - already migrated above.
			if path == root {
				return nil
			}

			base := filepath.Base(path)

			// Check for .crush directory BEFORE exclusion rules.
			// This ensures a gitignored .crush is still migrated.
			if d.IsDir() && base == ".crush" {
				dirStatus, innerStatus, newDir := migrateDir(cmd, path, dryRun, "project:")
				switch dirStatus {
				case statusRenamed:
					renamedCount++
					foundAny = true
				case statusRefused:
					refusedCount++
					foundAny = true
				case statusFailed:
					failedCount++
					foundAny = true
				}
				switch innerStatus {
				case statusRenamed:
					renamedCount++
					foundAny = true
				case statusRefused:
					refusedCount++
					foundAny = true
				case statusFailed:
					failedCount++
					foundAny = true
				}
				if dirStatus == statusRenamed {
					artRenamed, artRefused, artFailed := migrateKnownArtifacts(cmd, path, newDir, dryRun, "project:")
					renamedCount += artRenamed
					refusedCount += artRefused
					failedCount += artFailed
					foundAny = foundAny || artRenamed > 0 || artRefused > 0 || artFailed > 0
				}
				// Never descend into a .crush directory.
				return filepath.SkipDir
			}

			// Skip ignored directories (but NOT .crush, handled above).
			if d.IsDir() && fsext.ShouldExcludeFile(root, path) {
				return filepath.SkipDir
			}

			// Check for crush.json file.
			if !d.IsDir() && base == "crush.json" {
				status, _ := migrateFile(cmd, path, dryRun, "project:")
				switch status {
				case statusRenamed:
					renamedCount++
					foundAny = true
				case statusRefused:
					refusedCount++
					foundAny = true
				case statusFailed:
					failedCount++
					foundAny = true
				}
			}

			return nil
		})
		if err != nil {
			cmd.Printf("walk completed with error: %v\n", err)
		}
	} else {
		// Single-dir mode: handle root's .crush/ and crush.json.
		rootRenamed, rootRefused, rootFailed, rootFoundAny := migrateRootLocation(cmd, root, dryRun)
		renamedCount += rootRenamed
		refusedCount += rootRefused
		failedCount += rootFailed
		foundAny = foundAny || rootFoundAny
	}

	// tallyGlobal folds a migrateGlobalLocation result into the running
	// counters, shared by the config/data/system-config call sites below.
	tallyGlobal := func(dirStatus, innerStatus migrateStatus, artRenamed, artRefused, artFailed int) {
		for _, status := range [2]migrateStatus{dirStatus, innerStatus} {
			switch status {
			case statusRenamed:
				renamedCount++
				foundAny = true
			case statusRefused:
				refusedCount++
				foundAny = true
			case statusFailed:
				failedCount++
				foundAny = true
			}
		}
		renamedCount += artRenamed
		refusedCount += artRefused
		failedCount += artFailed
		foundAny = foundAny || artRenamed > 0 || artRefused > 0 || artFailed > 0
	}

	// Migrate global config location.
	legacyConfigPath := legacyGlobalConfigPath()
	currentConfigPath := config.GlobalConfig()
	tallyGlobal(migrateGlobalLocation(cmd, legacyConfigPath, currentConfigPath, dryRun, "global config:"))

	// Migrate global data location.
	legacyDataPath := legacyGlobalDataPath()
	currentDataPath := config.GlobalConfigData()
	tallyGlobal(migrateGlobalLocation(cmd, legacyDataPath, currentDataPath, dryRun, "global data:"))

	// Migrate system-wide config location (Unix only). Both
	// legacySystemConfigPath() and config.SystemConfig() are empty on
	// Windows, where there is no system-wide config location — skip the
	// call entirely rather than passing empty paths through
	// migrateGlobalLocation, whose filepath.Dir("") would resolve to "."
	// and behave unpredictably.
	legacySystemPath := legacySystemConfigPath()
	currentSystemPath := config.SystemConfig()
	if legacySystemPath != "" && currentSystemPath != "" {
		tallyGlobal(migrateGlobalLocation(cmd, legacySystemPath, currentSystemPath, dryRun, "system config:"))
	}

	// Print summary.
	if !foundAny {
		cmd.Printf("Nothing to migrate in %s\n", root)
	}

	if dryRun {
		cmd.Printf("summary: %d renamed, %d conflict — dry-run: no changes were made\n", renamedCount, refusedCount)
	} else {
		cmd.Printf("summary: %d renamed, %d conflict, %d failed\n", renamedCount, refusedCount, failedCount)
	}

	// Exit non-zero on any conflict or failure in a real run.
	if !dryRun {
		if refusedCount > 0 && failedCount > 0 {
			return fmt.Errorf("%d item(s) refused due to conflicts, %d item(s) failed — resolve manually", refusedCount, failedCount)
		} else if refusedCount > 0 {
			return fmt.Errorf("%d item(s) refused due to conflicts — resolve manually", refusedCount)
		} else if failedCount > 0 {
			return fmt.Errorf("%d item(s) failed — review output for details", failedCount)
		}
	}
	return nil
}

// migrateRootLocation migrates a single directory's root-level .crush/ and
// crush.json (project-scoped, not global). It is shared by the recursive
// and non-recursive branches of runMigrate: the recursive branch's
// filepath.WalkDir skips the root itself (renaming .crush -> .rush for the
// root mid-walk would otherwise interact badly with WalkDir's traversal of
// that same path), so the root always needs this separate, explicit call.
// Returns the renamed/refused/failed counts to fold into the caller's
// running totals, plus whether anything was found (for the "nothing to
// migrate" summary line).
func migrateRootLocation(cmd *cobra.Command, root string, dryRun bool) (renamed, refused, failed int, foundAny bool) {
	crushDir := filepath.Join(root, ".crush")
	crushFile := filepath.Join(root, "crush.json")

	tally := func(status migrateStatus) {
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

	dirStatus, innerStatus, newDir := migrateDir(cmd, crushDir, dryRun, "project:")
	tally(dirStatus)
	tally(innerStatus)
	if dirStatus == statusRenamed {
		artRenamed, artRefused, artFailed := migrateKnownArtifacts(cmd, crushDir, newDir, dryRun, "project:")
		renamed += artRenamed
		refused += artRefused
		failed += artFailed
		foundAny = foundAny || artRenamed > 0 || artRefused > 0 || artFailed > 0
	}

	fileStatus, _ := migrateFile(cmd, crushFile, dryRun, "project:")
	tally(fileStatus)

	return renamed, refused, failed, foundAny
}

// legacyGlobalConfigPath returns the legacy Crush global config path.
// Precedence: CRUSH_GLOBAL_CONFIG → RUSH_GLOBAL_CONFIG → XDG_CONFIG_HOME-or-~/.config/crush/crush.json.
//
// CRUSH_* env vars take precedence over RUSH_* env vars because the legacy
// location should be resolved using the old names first, falling back to the
// new names only if the user has already partially migrated (e.g., set
// RUSH_GLOBAL_CONFIG to the legacy path for testing).
func legacyGlobalConfigPath() string {
	if crushGlobal := os.Getenv("CRUSH_GLOBAL_CONFIG"); crushGlobal != "" {
		return filepath.Join(crushGlobal, "crush.json")
	}
	if rushGlobal := os.Getenv("RUSH_GLOBAL_CONFIG"); rushGlobal != "" {
		return filepath.Join(rushGlobal, "crush.json")
	}
	return filepath.Join(home.Config(), "crush", "crush.json")
}

// legacyGlobalDataPath returns the legacy Crush global data path.
// Precedence: CRUSH_GLOBAL_DATA → RUSH_GLOBAL_DATA → XDG_DATA_HOME/crush/crush.json
// → %LOCALAPPDATA%/crush/crush.json (Windows) → ~/.local/share/crush/crush.json (Unix).
//
// CRUSH_* env vars take precedence over RUSH_* env vars for the same reason
// as in legacyGlobalConfigPath: resolve the legacy location first.
func legacyGlobalDataPath() string {
	if crushData := os.Getenv("CRUSH_GLOBAL_DATA"); crushData != "" {
		return filepath.Join(crushData, "crush.json")
	}
	if rushData := os.Getenv("RUSH_GLOBAL_DATA"); rushData != "" {
		return filepath.Join(rushData, "crush.json")
	}
	if xdgDataHome := os.Getenv("XDG_DATA_HOME"); xdgDataHome != "" {
		return filepath.Join(xdgDataHome, "crush", "crush.json")
	}
	// Windows: %LOCALAPPDATA%/crush/crush.json
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		return filepath.Join(localAppData, "crush", "crush.json")
	}
	if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
		return filepath.Join(userProfile, "AppData", "Local", "crush", "crush.json")
	}
	// Unix: ~/.local/share/crush/crush.json
	return filepath.Join(home.Dir(), ".local", "share", "crush", "crush.json")
}

// legacySystemConfigPath returns the legacy Crush system-wide config path
// (/etc/crush/crush.json). It mirrors config.SystemConfig()'s Unix-only
// gating: empty on Windows, where no system-wide config location exists
// (see internal/config/config_unix.go and config_windows.go). A runtime
// check is used here, rather than a build-tag-gated file like config_unix.go,
// because this is a single constant path with no other Unix-only logic
// attached — a whole extra file pair would be disproportionate.
func legacySystemConfigPath() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return "/etc/crush/crush.json"
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
		// Check for conflict with target file in the new directory.
		if _, err := os.Stat(newInnerConfigPath); err == nil {
			cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", configPrefix, legacyInnerConfig, newInnerConfigPath)
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

	// Build the new file path.
	parent := filepath.Dir(oldFile)
	base := filepath.Base(oldFile)
	var newFile string
	if base == "crush.json" {
		newFile = filepath.Join(parent, "rush.json")
	} else {
		// Unexpected file name - this shouldn't happen in normal use.
		return statusFailed, ""
	}

	// Check for conflict: new file already exists.
	if _, err := os.Stat(newFile); err == nil {
		cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", formatPrefix(prefix), oldFile, newFile)
		return statusRefused, ""
	}

	// Perform or report the file rename.
	if dryRun {
		cmd.Printf("would rename %s%s  ->  %s\n", formatPrefix(prefix), oldFile, newFile)
		rewriteLegacyConfigContent(cmd, oldFile, newFile, dryRun, prefix)
		return statusRenamed, newFile
	}

	if err := os.Rename(oldFile, newFile); err != nil {
		cmd.Printf("failed to rename %s%s  ->  %s: %v\n", formatPrefix(prefix), oldFile, newFile, err)
		return statusFailed, ""
	}

	cmd.Printf("renamed %s%s  ->  %s\n", formatPrefix(prefix), oldFile, newFile)
	rewriteLegacyConfigContent(cmd, newFile, newFile, dryRun, prefix)
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
// readFrom is the path to read current content from: in a real run this is
// the already-renamed newFile; in dry-run, nothing has moved yet, so the
// caller passes the still-in-place oldFile instead (same convention
// migrateDir/migrateKnownArtifacts use elsewhere in this file). reportAs is
// always the newFile path, used only for log messages.
//
// Errors reading or parsing the file are reported but non-fatal to the
// overall migration - the file has already been renamed (the primary goal),
// content rewriting is a best-effort follow-up.
func rewriteLegacyConfigContent(cmd *cobra.Command, readFrom, reportAs string, dryRun bool, prefix string) {
	raw, err := os.ReadFile(readFrom)
	if err != nil {
		// Nothing to rewrite if we can't read it; migrateFile already
		// reported the rename itself.
		return
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		// Not a JSON object we understand - leave content untouched.
		return
	}

	optionsRaw, ok := top["options"]
	if !ok {
		return
	}

	var options map[string]json.RawMessage
	if err := json.Unmarshal(optionsRaw, &options); err != nil {
		return
	}

	contentPrefix := formatPrefix(prefix, "(content inside migrated config)")
	changed := false
	var notes []string

	// disabled_skills: exact-match rewrite of known legacy skill IDs.
	if rawSkills, ok := options["disabled_skills"]; ok {
		var skills []string
		if err := json.Unmarshal(rawSkills, &skills); err == nil {
			rewritten := false
			for i, name := range skills {
				if newName, isLegacy := legacySkillRenames[name]; isLegacy {
					skills[i] = newName
					rewritten = true
					notes = append(notes, fmt.Sprintf("disabled_skills: %q -> %q", name, newName))
				}
			}
			if rewritten {
				if newRaw, err := json.Marshal(skills); err == nil {
					options["disabled_skills"] = newRaw
					changed = true
				}
			}
		}
	}

	// skills_paths, global_context_paths: path-segment-aware .crush ->
	// .rush rewrite, plus CRUSH.md -> RUSH.md for the context-file case.
	for _, field := range []string{"skills_paths", "global_context_paths"} {
		rawPaths, ok := options[field]
		if !ok {
			continue
		}
		var paths []string
		if err := json.Unmarshal(rawPaths, &paths); err != nil {
			continue
		}
		rewritten := false
		for i, p := range paths {
			newPath := rewriteLegacyConfigPath(p)
			if newPath != p {
				paths[i] = newPath
				rewritten = true
				notes = append(notes, fmt.Sprintf("%s: %q -> %q", field, p, newPath))
			}
		}
		if rewritten {
			if newRaw, err := json.Marshal(paths); err == nil {
				options[field] = newRaw
				changed = true
			}
		}
	}

	// data_directory: same .crush-path-segment rewrite, only if set.
	if rawDataDir, ok := options["data_directory"]; ok {
		var dataDir string
		if err := json.Unmarshal(rawDataDir, &dataDir); err == nil && dataDir != "" {
			newDataDir := rewriteLegacyConfigPath(dataDir)
			if newDataDir != dataDir {
				if newRaw, err := json.Marshal(newDataDir); err == nil {
					options["data_directory"] = newRaw
					changed = true
					notes = append(notes, fmt.Sprintf("data_directory: %q -> %q", dataDir, newDataDir))
				}
			}
		}
	}

	if !changed {
		return
	}

	if dryRun {
		for _, note := range notes {
			cmd.Printf("would rewrite %s%s: %s\n", contentPrefix, reportAs, note)
		}
		return
	}

	newOptionsRaw, err := json.Marshal(options)
	if err != nil {
		cmd.Printf("failed to rewrite content %s%s: %v\n", contentPrefix, reportAs, err)
		return
	}
	top["options"] = newOptionsRaw

	newRaw, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		cmd.Printf("failed to rewrite content %s%s: %v\n", contentPrefix, reportAs, err)
		return
	}
	newRaw = append(newRaw, '\n')

	if err := os.WriteFile(reportAs, newRaw, 0o644); err != nil {
		cmd.Printf("failed to rewrite content %s%s: %v\n", contentPrefix, reportAs, err)
		return
	}

	for _, note := range notes {
		cmd.Printf("rewrote %s%s: %s\n", contentPrefix, reportAs, note)
	}
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

	dbStatus, _ := migrateNamedFile(cmd, filepath.Join(sourceDir, "crush.db"), filepath.Join(newDir, "rush.db"), dryRun, artifactPrefix)
	tally(dbStatus)

	sourceLogsDir := filepath.Join(sourceDir, "logs")
	if _, err := os.Stat(sourceLogsDir); err == nil {
		newLogsDir := filepath.Join(newDir, "logs")
		logStatus, _ := migrateNamedFile(cmd, filepath.Join(sourceLogsDir, "crush.log"), filepath.Join(newLogsDir, "rush.log"), dryRun, artifactPrefix)
		tally(logStatus)
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

// migrateGlobalLocation migrates a global config or data location.
// Handles two cases:
// 1. Whole directory rename (when target directory doesn't exist).
// 2. File-level rename (when both directories exist or are the same).
//
// Returns the status of the directory/file migration, the status of the
// inner config migration, and the renamed/refused/failed counts of known
// artifact files (crush.db, logs/crush.log) migrated alongside a whole
// directory rename (case 1 only — case 2's file-level migration has no
// directory to move artifacts within, so it always returns zero counts).
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
	} else {
		// Directory has other contents - user must handle manually.
		cmd.Printf("notice: legacy directory %s still contains other files (e.g., auth.json, skills) that were NOT merged. Copy or move them manually, then delete the old directory.\n", legacyDir)
		// Count as refused so exit code reflects unfinished work.
		return statusRefused, statusNone, 0, 0, 0
	}
}
