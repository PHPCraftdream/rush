package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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

Loose legacy context and ignore files are also renamed to their Rush
equivalents wherever encountered (project root always; every directory visited
during a --recursive walk for .crushignore, since it is read per-directory):
CRUSH.md/CRUSH.local.md (and the Crush.md/Rush.md case variants the app's
context-file loader recognizes) → RUSH.md/RUSH.local.md, and
.crushignore → .rushignore.

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
    location is often root-owned; migrating it may require elevated privileges.

If a global location's target directory already exists (e.g. because the app itself
already created ~/.config/rush on a prior run), only crush.json is renamed by default;
any other legacy files left behind (skills/, auth.json, etc.) are then also moved into
the target directory individually, refusing only the specific items whose names already
exist there (everything else still moves) - never a blanket "leave it all behind".

At the end of the run, a "Manual follow-up needed" section reports what this command
structurally cannot fix on its own: any CRUSH_*-prefixed environment variable still set
in your shell that has no automatic Rush equivalent (the app itself only reads RUSH_*
names now), and a reminder to run the matching *-del command (claude-del, codex-del,
gemini-del, grok-del, qwen-del) if you still have old crush-named slash-commands/agents
installed by a pre-rename *-init run.`,
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
				renamedCount += ctxRenamed
				refusedCount += ctxRefused
				failedCount += ctxFailed
				foundAny = foundAny || ctxRenamed > 0 || ctxRefused > 0 || ctxFailed > 0

				gitignoreAdded := updateGitignoreForMigratedProject(cmd, path, dryRun, "project:")
				foundAny = foundAny || gitignoreAdded > 0
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

	printManualFollowUp(cmd)

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

// handledLegacyEnvVars are the CRUSH_*-prefixed environment variables that
// migrate itself already looks at (as a legacy-lookup fallback, to locate
// pre-rename global/data paths — see legacyGlobalConfigPath and
// legacyGlobalDataPath). They must NOT be re-listed in the "manual
// follow-up needed" report below, since migrate already accounts for them.
var handledLegacyEnvVars = map[string]bool{
	"CRUSH_GLOBAL_CONFIG": true,
	"CRUSH_GLOBAL_DATA":   true,
}

// printManualFollowUp prints a distinct, clearly-labeled report of cleanup
// steps that a filesystem-renaming tool like migrate structurally cannot
// perform on its own:
//
//  1. Any CRUSH_*-prefixed environment variable currently set in this
//     process's environment that migrate does NOT already look at as a
//     legacy fallback (see handledLegacyEnvVars). The running app itself
//     now only checks RUSH_*-named variables (e.g. RUSH_CACHE_DIR,
//     RUSH_SKILLS_DIR, RUSH_FORBID_WRITES) with no CRUSH_* fallback, so an
//     old CRUSH_* variable still set in the user's shell profile is
//     silently ignored rather than erroring - easy to miss. This scans the
//     actual process environment (os.Environ()) rather than a static list,
//     so it is accurate to what THIS user's shell actually has set.
//  2. A fixed reminder about the matching *-del command(s), for cleaning up
//     old crush-named slash-commands/agents left behind by a pre-rename
//     crush-init run. This part is deliberately static: migrate does not
//     scan disk for leftover crush-named installed files (out of scope),
//     it just reminds the user that *-del exists and does that job.
func printManualFollowUp(cmd *cobra.Command) {
	var staleEnvVars []string
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || value == "" || !strings.HasPrefix(name, "CRUSH_") {
			// An empty value is treated as unset, same convention this file
			// already uses for legacy env var lookups elsewhere (see
			// legacyGlobalConfigPath/legacyGlobalDataPath's `!= ""` checks).
			continue
		}
		if handledLegacyEnvVars[name] {
			continue
		}
		staleEnvVars = append(staleEnvVars, name)
	}
	sort.Strings(staleEnvVars)

	cmd.Printf("\nManual follow-up needed:\n")

	if len(staleEnvVars) > 0 {
		cmd.Printf("  - Legacy environment variable(s) still set in this process's environment with no automatic Rush equivalent: %s. The app no longer reads these (only the matching RUSH_* names are checked) - update your shell profile (.bashrc/.zshrc/PowerShell profile/etc.) to rename them.\n", strings.Join(staleEnvVars, ", "))
	} else {
		cmd.Printf("  - No stray CRUSH_* environment variables detected in this process's environment.\n")
	}

	cmd.Printf("  - If you previously ran a pre-rename crush-init/codex-init/gemini-init/grok-init/qwen-init, old crush-named slash-commands/agents are left behind - this migrate command does not touch them. Run the matching *-del command (e.g. claude-del, codex-del, gemini-del, grok-del, qwen-del) to remove them, then re-run the corresponding *-init to reinstall the current rush-named versions.\n")
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

	ctxRenamed, ctxRefused, ctxFailed := migrateContextAndIgnoreFiles(cmd, root, dryRun, "project:")
	renamed += ctxRenamed
	refused += ctxRefused
	failed += ctxFailed
	foundAny = foundAny || ctxRenamed > 0 || ctxRefused > 0 || ctxFailed > 0

	gitignoreAdded := updateGitignoreForMigratedProject(cmd, root, dryRun, "project:")
	foundAny = foundAny || gitignoreAdded > 0

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
// readFrom is the path to read current content from: in a real run this is
// the already-renamed newFile; in dry-run, nothing has moved yet, so the
// caller passes the still-in-place oldFile instead (same convention
// migrateDir/migrateKnownArtifacts use elsewhere in this file). reportAs is
// always the newFile path, used both for log messages and as the write
// target.
//
// Returns statusFailed only for an actual I/O error while writing the
// rewritten content back (see the atomic-write step at the end of this
// function) - by that point the caller's rename has already happened and
// there is no fallback copy left, so the caller folds this into its own
// overall status rather than reporting success. Errors reading or parsing
// readFrom, or there being nothing to rewrite, are reported as statusNone:
// non-fatal, since the file has already been renamed (the primary goal) and
// there is nothing this function could have written anyway.
func rewriteLegacyConfigContent(cmd *cobra.Command, readFrom, reportAs string, dryRun bool, prefix string) migrateStatus {
	raw, err := os.ReadFile(readFrom)
	if err != nil {
		// Nothing to rewrite if we can't read it; migrateFile already
		// reported the rename itself.
		return statusNone
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		// Not a JSON object we understand - leave content untouched.
		return statusNone
	}

	optionsRaw, ok := top["options"]
	if !ok {
		return statusNone
	}

	var options map[string]json.RawMessage
	if err := json.Unmarshal(optionsRaw, &options); err != nil {
		return statusNone
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
		return statusNone
	}

	if dryRun {
		for _, note := range notes {
			cmd.Printf("would rewrite %s%s: %s\n", contentPrefix, reportAs, note)
		}
		return statusNone
	}

	newOptionsRaw, err := json.Marshal(options)
	if err != nil {
		cmd.Printf("failed to rewrite content %s%s: %v\n", contentPrefix, reportAs, err)
		return statusFailed
	}
	top["options"] = newOptionsRaw

	newRaw, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		cmd.Printf("failed to rewrite content %s%s: %v\n", contentPrefix, reportAs, err)
		return statusFailed
	}
	newRaw = append(newRaw, '\n')

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

	if _, err := tmpFile.Write(newRaw); err != nil {
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
