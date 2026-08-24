package cmd

import (
	"fmt"
	"os"
	"path/filepath"

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
starting at the given root.

A --dry-run reports what would happen without making any changes.

Conflict rule: if the target path already exists, the item is refused with a clear
message and left untouched. Never clobbers existing Rush files or directories.

Ignored directories (.git, node_modules, vendor, .rush, etc.) are skipped during
recursive walks using gitignore/common-pattern-aware exclusion rules. However,
a .crush directory that matches ignore patterns is still migrated (the name check
happens before the ignore check).

Files inside a migrated .crush/ other than crush.json (e.g. legacy crush.db) are
left as-is and not touched.

Global locations (always processed, regardless of --recursive):
  - Config: ~XDG_CONFIG_HOME-or-~/.config/crush/crush.json → rush/rush.json
  - Data: XDG_DATA_HOME/crush/crush.json → rush/rush.json (or %LOCALAPPDATA%/crush/ on Windows)`,
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
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				// Record walk error but continue where possible.
				cmd.Printf("walk error: %v\n", err)
				failedCount++
				return nil
			}

			// Skip the root itself - handle it separately below.
			if path == root {
				return nil
			}

			base := filepath.Base(path)

			// Check for .crush directory BEFORE exclusion rules.
			// This ensures a gitignored .crush is still migrated.
			if d.IsDir() && base == ".crush" {
				dirStatus, innerStatus, _ := migrateDir(cmd, path, dryRun, "project:")
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
		crushDir := filepath.Join(root, ".crush")
		crushFile := filepath.Join(root, "crush.json")

		dirStatus, innerStatus, _ := migrateDir(cmd, crushDir, dryRun, "project:")
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

		var status migrateStatus
		status, _ = migrateFile(cmd, crushFile, dryRun, "project:")
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

	// Migrate global config location.
	var configDirStatus, configInnerStatus migrateStatus
	legacyConfigPath := legacyGlobalConfigPath()
	currentConfigPath := config.GlobalConfig()
	configDirStatus, configInnerStatus = migrateGlobalLocation(cmd, legacyConfigPath, currentConfigPath, dryRun, "global config:")
	switch configDirStatus {
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
	switch configInnerStatus {
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

	// Migrate global data location.
	var dataDirStatus, dataInnerStatus migrateStatus
	legacyDataPath := legacyGlobalDataPath()
	currentDataPath := config.GlobalConfigData()
	dataDirStatus, dataInnerStatus = migrateGlobalLocation(cmd, legacyDataPath, currentDataPath, dryRun, "global data:")
	switch dataDirStatus {
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
	switch dataInnerStatus {
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
// Returns the status of the directory/file migration and the status of the inner config migration.
//
// When renaming just the file and the legacy directory becomes empty, it is
// removed. If the directory still has other contents (e.g., auth.json, skills),
// a notice is printed and the operation is counted as refused (unfinished
// manual work remains).
func migrateGlobalLocation(cmd *cobra.Command, legacyPath, currentPath string, dryRun bool, prefix string) (migrateStatus, migrateStatus) {
	legacyDir := filepath.Dir(legacyPath)
	rushDir := filepath.Dir(currentPath)

	// Check if legacy directory exists.
	if _, err := os.Stat(legacyDir); os.IsNotExist(err) {
		return statusNone, statusNone
	}

	// If legacy and rush directories are the same (env-var-isolated case),
	// reduce to file-level migration.
	if legacyDir == rushDir {
		status, _ := migrateFile(cmd, legacyPath, dryRun, prefix)
		return status, statusNone
	}

	// Case 1: target directory doesn't exist - rename whole directory.
	if _, err := os.Stat(rushDir); os.IsNotExist(err) {
		if dryRun {
			cmd.Printf("would rename %s%s  ->  %s\n", formatPrefix(prefix), legacyDir, rushDir)
			// Check for inner config in dry-run mode.
			innerConfigPath := filepath.Join(legacyDir, "crush.json")
			newInnerConfigPath := filepath.Join(rushDir, "rush.json")
			configPrefix := formatPrefix(prefix, "(config inside migrated directory)")
			if _, err := os.Stat(innerConfigPath); !os.IsNotExist(err) {
				if _, err := os.Stat(newInnerConfigPath); err == nil {
					cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", configPrefix, innerConfigPath, newInnerConfigPath)
					return statusRenamed, statusRefused
				}
				cmd.Printf("would rename %s%s  ->  %s\n", configPrefix, innerConfigPath, newInnerConfigPath)
				return statusRenamed, statusRenamed
			}
			return statusRenamed, statusNone
		}
		if err := os.Rename(legacyDir, rushDir); err != nil {
			cmd.Printf("failed to rename %s%s  ->  %s: %v\n", formatPrefix(prefix), legacyDir, rushDir, err)
			return statusFailed, statusNone
		}
		cmd.Printf("renamed %s%s  ->  %s\n", formatPrefix(prefix), legacyDir, rushDir)

		// Migrate the inner config file.
		configPrefix := formatPrefix(prefix, "(config inside migrated directory)")
		innerStatus, _ := migrateFile(cmd, filepath.Join(rushDir, "crush.json"), dryRun, configPrefix)
		return statusRenamed, innerStatus
	}

	// Case 2: target directory exists - file-level migration.
	// Check if legacy file exists.
	if _, err := os.Stat(legacyPath); os.IsNotExist(err) {
		return statusNone, statusNone
	}

	// Check if target file exists (conflict).
	if _, err := os.Stat(currentPath); err == nil {
		cmd.Printf("CONFLICT %s%s  ->  %s: target already exists — resolve manually (merge by hand, then delete the old path); refusing to touch either\n", formatPrefix(prefix), legacyPath, currentPath)
		return statusRefused, statusNone
	}

	// Perform or report the file rename.
	if dryRun {
		cmd.Printf("would rename %s%s  ->  %s\n", formatPrefix(prefix), legacyPath, currentPath)
		return statusRenamed, statusNone
	}

	if err := os.Rename(legacyPath, currentPath); err != nil {
		cmd.Printf("failed to rename %s%s  ->  %s: %v\n", formatPrefix(prefix), legacyPath, currentPath, err)
		return statusFailed, statusNone
	}

	cmd.Printf("renamed %s%s  ->  %s\n", formatPrefix(prefix), legacyPath, currentPath)

	// Check if legacy directory is now empty.
	entries, err := os.ReadDir(legacyDir)
	if err != nil {
		// Failed to read directory - can't determine emptiness, leave it.
		return statusRenamed, statusNone
	}

	if len(entries) == 0 {
		// Directory is empty - remove it.
		if err := os.Remove(legacyDir); err != nil {
			cmd.Printf("notice: failed to remove now-empty legacy directory %s: %v\n", legacyDir, err)
		}
		return statusRenamed, statusNone
	} else {
		// Directory has other contents - user must handle manually.
		cmd.Printf("notice: legacy directory %s still contains other files (e.g., auth.json, skills) that were NOT merged. Copy or move them manually, then delete the old directory.\n", legacyDir)
		// Count as refused so exit code reflects unfinished work.
		return statusRefused, statusNone
	}
}
