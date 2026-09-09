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

// migrateFileRenamePauseSeam is a test-only hook that fires right after
// migrateFile's create-rename (oldFile -> newFile) has succeeded, before
// rewriteLegacyConfigContent runs. nil (a no-op) in every production path.
// Lets a test deterministically synchronize a concurrent process against
// the exact moment newFile first exists on disk, instead of racing a
// busy-poll loop against migrateFile's own execution speed (see
// TestMigrateCLITallyReflectsRewriteFailure).
var migrateFileRenamePauseSeam func()

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
