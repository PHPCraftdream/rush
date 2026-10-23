// Ktav/JSON logical-slot discovery: every place that names a legacy
// rush.json config resolves to exactly one selected file per slot,
// preferring rush.ktav, and read documents are decoded from Ktav to
// JSON-shaped bytes before the existing merge runs.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
)

const (
	// ktavConfigExt is the preferred disk extension for Rush-owned config.
	ktavConfigExt = ".ktav"
	// ktavBackupSuffix marks a retired legacy JSON original.
	ktavBackupSuffix = ".bak"
	// ktavMigrationRecordSuffix marks an in-progress or completed migration
	// transaction beside its legacy source.
	ktavMigrationRecordSuffix = ".ktav-migration"
)

var (
	// errKtavBackupOnly reports a well-known config location that holds
	// only a .bak backup. Backups are never restored automatically.
	errKtavBackupOnly = errors.New("config backup exists without an active config file; restore it manually")
	// errKtavMigrationConflict reports a disk state the migration cannot
	// prove safe; every entry is preserved for manual resolution.
	errKtavMigrationConflict = errors.New("ktav config migration stopped; manual resolution required")
	// errKtavMigrationRecord reports a migration record that exists but
	// cannot be authenticated for the slot it names.
	errKtavMigrationRecord = errors.New("ktav migration record is invalid or foreign; preserve it and resolve manually")
)

// ktavSiblingOf returns the preferred Ktav spelling of a legacy JSON path.
func ktavSiblingOf(legacyJSON string) string {
	return strings.TrimSuffix(legacyJSON, ".json") + ktavConfigExt
}

// backupPathFor returns the retained backup name for a legacy JSON path:
// rush.json.bak, .rush.json.bak. Nothing is stripped.
func backupPathFor(legacyJSON string) string {
	return legacyJSON + ktavBackupSuffix
}

// migrationRecordPathFor returns the transaction record path for a legacy
// JSON source. The record and the backup anchor on the LEGACY path, never
// on rush.ktav.
func migrationRecordPathFor(legacyJSON string) string {
	return legacyJSON + ktavMigrationRecordSuffix
}

// isKtavConfigPath reports whether path names a Ktav-format config file.
func isKtavConfigPath(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ktavConfigExt)
}

// isKtavBackupPath reports whether path names a retained backup of a
// legacy JSON config.
func isKtavBackupPath(path string) bool {
	return strings.HasSuffix(path, ktavBackupSuffix)
}

// isKtavMigrationRecordPath reports whether path names a migration
// transaction record.
func isKtavMigrationRecordPath(path string) bool {
	return strings.HasSuffix(path, ktavMigrationRecordSuffix)
}

var ktavBothFormatsWarned sync.Map

// warnBothFormatsOnce logs the both-formats condition once per logical
// source. Messages carry paths only, never document values.
func warnBothFormatsOnce(ktav, legacy string) {
	if _, dup := ktavBothFormatsWarned.LoadOrStore(ktav, struct{}{}); dup {
		return
	}
	slog.Warn("Both config formats exist; using Ktav and ignoring legacy JSON",
		"ktav", ktav, "json", legacy)
}

// warnMigrationDeferredOnce logs a migration that could not run while the
// valid legacy JSON keeps serving. Messages carry paths only.
func warnMigrationDeferredOnce(legacy, reason string) {
	if _, dup := ktavBothFormatsWarned.LoadOrStore("deferred:"+legacy, struct{}{}); dup {
		return
	}
	slog.Warn("Ktav config migration deferred; continuing with legacy JSON",
		"path", legacy, "reason", reason)
}

// selectConfigSlot picks exactly one file for the logical slot named by a
// legacy JSON path: rush.ktav when it exists (warning when rush.json is
// also present), otherwise the legacy name. An empty legacy path means
// "no slot" (Windows system config) and never probes a relative name. A
// Ktav file that exists is always selected, even when ownership policy
// will reject it, so a foreign-owned Ktav file never silently downgrades
// the slot to JSON.
func selectConfigSlot(legacyJSON string) string {
	if legacyJSON == "" {
		return ""
	}
	ktav := ktavSiblingOf(legacyJSON)
	info, err := os.Stat(ktav)
	if err != nil && !os.IsNotExist(err) {
		// Unreadable parent or I/O error: keep the Ktav spelling so the
		// stable reader surfaces the real error instead of silently
		// falling back to JSON.
		return ktav
	}
	if err == nil {
		if !info.IsDir() {
			if _, jsonErr := os.Stat(legacyJSON); jsonErr == nil {
				warnBothFormatsOnce(ktav, legacyJSON)
			}
		}
		return ktav
	}
	return legacyJSON
}

// decodeConfigDocument converts a selected .ktav document's raw bytes into
// the JSON-shaped bytes the merge and semantic decoders expect. Fingerprints
// are always computed from the raw file bytes, never from this output.
func decodeConfigDocument(path string, data []byte) ([]byte, error) {
	if !isKtavConfigPath(path) {
		return data, nil
	}
	tree, err := ktavcodec.DecodeKtav(data)
	if err != nil {
		return nil, fmt.Errorf("invalid Ktav in config file %s: %w", path, err)
	}
	jsonBytes, err := ktavcodec.ToJSON(tree)
	if err != nil {
		return nil, fmt.Errorf("invalid Ktav in config file %s: %w", path, err)
	}
	return jsonBytes, nil
}

// workspaceConfigPath resolves the preferred config spelling inside a data
// directory: <dir>/rush.ktav when it exists, otherwise <dir>/rush.json.
func workspaceConfigPath(dataDir string) string {
	return selectConfigSlot(filepath.Join(dataDir, appName+".json"))
}

// matchesLogicalSlot reports whether canonical names any serialization of
// the logical slot anchored at legacyJSON (the legacy JSON or its Ktav
// sibling). Backup and transaction-record spellings are never write targets.
func matchesLogicalSlot(canonical, legacyJSON string) bool {
	if legacyJSON == "" {
		return false
	}
	return canonical == normalizeReloadPath(legacyJSON) ||
		canonical == normalizeReloadPath(ktavSiblingOf(legacyJSON))
}

// checkBackupOnlySlot fails closed for an authoritative config location
// that holds only a retained backup: treating it as an unlocked, empty
// config would silently discard the operator's settings.
func checkBackupOnlySlot(legacyJSON string) error {
	if legacyJSON == "" {
		return nil
	}
	if ktavEntryExists(legacyJSON) || ktavEntryExists(ktavSiblingOf(legacyJSON)) {
		return nil
	}
	backup := backupPathFor(legacyJSON)
	if !ktavEntryExists(backup) {
		return nil
	}
	return fmt.Errorf("%w: %s exists without %s or %s",
		errKtavBackupOnly, backup, filepath.Base(legacyJSON), filepath.Base(ktavSiblingOf(legacyJSON)))
}

// checkAuthoritativeBackupOnly applies checkBackupOnlySlot to every
// well-known authoritative config location, including every project slot
// the bounded discovery walk reports (visible and hidden, every ancestor).
func checkAuthoritativeBackupOnly(workingDir string) error {
	if systemConfigPath != "" {
		if err := checkBackupOnlySlot(systemConfigPath); err != nil {
			return err
		}
	}
	for _, slot := range []string{GlobalConfig(), GlobalConfigData()} {
		if err := checkBackupOnlySlot(slot); err != nil {
			return err
		}
	}
	for _, slot := range ktavProjectLegacySlots(workingDir) {
		if err := checkBackupOnlySlot(slot); err != nil {
			return err
		}
	}
	return nil
}

func ktavProjectLegacySlots(workingDir string) []string {
	if workingDir == "" {
		return nil
	}
	abs := canonicalConfigPath(workingDir)
	boundary := canonicalConfigPath(projectBoundary(abs))
	owner, err := fsext.Owner(abs)
	if err != nil {
		return nil
	}
	var slots []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		dirOwner, ownerErr := fsext.Owner(dir)
		if ownerErr != nil || !configOwnersMatch(owner, dirOwner) {
			break
		}
		for _, base := range []string{appName, "." + appName} {
			slots = append(slots, filepath.Join(dir, base+".json"))
		}
		if sameDir(dir, boundary) || filepath.Dir(dir) == dir {
			break
		}
	}
	return slots
}

// ktavMigrationPendingSlots reports legacy config paths whose migration
// transaction is incomplete: a record exists beside the legacy path while
// the active slot selected Ktav (or JSON). Finishing an interrupted
// transaction is always safe, regardless of the auto-migrate gate.
func ktavMigrationPendingSlots(workingDir string) []string {
	var pending []string
	seen := make(map[string]struct{})
	add := func(legacy string) {
		if legacy == "" || isKtavConfigPath(legacy) || isKtavBackupPath(legacy) {
			return
		}
		if _, dup := seen[legacy]; dup {
			return
		}
		seen[legacy] = struct{}{}
		if ktavEntryExists(migrationRecordPathFor(legacy)) {
			pending = append(pending, legacy)
		}
	}
	if systemConfigPath != "" {
		add(systemConfigPath)
	}
	for _, slot := range []string{GlobalConfig(), GlobalConfigData()} {
		add(slot)
	}
	for _, slot := range ktavProjectLegacySlots(workingDir) {
		add(slot)
	}
	return pending
}

func ktavEntryExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Lstat(path)
	// A path below a regular file (ENOTDIR) holds no entry either.
	return err == nil || (!os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR))
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
