package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/fs/home"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
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
		root, err = cmdkit.ResolveCwd(cmd)
		if err != nil {
			return fmt.Errorf("failed to resolve working directory: %w", err)
		}
	}

	// Normalize the root path to ensure consistent comparison.
	root = filepath.Clean(root)

	if !dryRun {
		if err := config.SettingsWriteAllowed(root); err != nil {
			return err
		}
	}

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

		walkRenamed, walkRefused, walkFailed, walkFoundAny, walkErr := migrateWalkTree(cmd, root, dryRun)
		renamedCount += walkRenamed
		refusedCount += walkRefused
		failedCount += walkFailed
		foundAny = foundAny || walkFoundAny
		if walkErr != nil {
			return walkErr
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
// (see internal/config/domains/datadir/systemconfig_unix.go and systemconfig_windows.go). A runtime
// check is used here, rather than a build-tag-gated file like config_unix.go,
// because this is a single constant path with no other Unix-only logic
// attached — a whole extra file pair would be disproportionate.
func legacySystemConfigPath() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return "/etc/crush/crush.json"
}

// NewMigrateCmd returns the rush migrate command tree, registering its flags
// exactly once (former schema.go registration; lane M5 of the sprint0 cmd
// split moved it next to the command it configures). The command object
// itself is a package var that only this constructor mutates.
var newMigrateCmdOnce sync.Once

func NewMigrateCmd() *cobra.Command {
	newMigrateCmdOnce.Do(func() {
		migrateCmd.Flags().BoolP("recursive", "r", false, "Recursively walk the directory tree")
		migrateCmd.Flags().Bool("dry-run", false, "Report what would happen without making changes")
	})
	return cmdkit.DeepFreshCmd(migrateCmd)
}
