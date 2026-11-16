// Ktav slot discovery and format-aware settings documents.
package docread

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/kit/fs/fsext"
	"github.com/PHPCraftdream/rush/internal/kit/text/ktavcodec"
)

// configFileIdentity gives docread the shared lower-level file identity type.
type configFileIdentity = fileid.FileIdentity

// ktavConfigExt is the preferred disk extension for Rush-owned config.
const (
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

// configSlotBaseNames returns the single, authoritative list of config
// slot base names discovery understands: the visible rush.* pair and the
// hidden .rush.* pair. Slot-sibling expansion MUST derive its names from
// this list so the two never drift apart.
func configSlotBaseNames() []string {
	return []string{cfgmodel.AppName, "." + cfgmodel.AppName}
}

// expandConfigSlotSiblings appends the alternate spelling of every path
// that names a Rush-owned logical config slot (rush.json / rush.ktav,
// including the hidden .rush.* spellings discovery walks):
// the Ktav sibling of each legacy JSON path and the legacy JSON sibling of
// each Ktav path. This keeps both spellings of a slot in the tracked and
// candidate path sets, so the disk-sync poller sees a foreign write to
// either spelling on natural ticks, even when slot selection flips after
// Load (migration renames rush.json to rush.ktav mid-session).
func expandConfigSlotSiblings(paths []string) []string {
	out := slices.Clone(paths)
	for _, path := range paths {
		if path == "" {
			continue
		}
		base := filepath.Base(path)
		for _, name := range configSlotBaseNames() {
			if base == name+".json" {
				out = append(out, strings.TrimSuffix(path, ".json")+ktavConfigExt)
			}
			if base == name+ktavConfigExt {
				out = append(out, strings.TrimSuffix(path, ktavConfigExt)+".json")
			}
		}
	}
	return out
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
// source and kind. Messages carry paths only, never document values.
func warnBothFormatsOnce(kind, ktav, legacy string) {
	key := kind + ":" + ktav
	if _, dup := ktavBothFormatsWarned.LoadOrStore(key, struct{}{}); dup {
		return
	}
	ktavInfo, ktavErr := os.Stat(ktav)
	legacyInfo, legacyErr := os.Stat(legacy)
	if kind == "config" && ktavErr == nil && legacyErr == nil && legacyInfo.ModTime().After(ktavInfo.ModTime()) {
		slog.Warn("rush.json changed after rush.ktav (by hand or an older rush); its edits are ignored",
			"ktav", ktav, "json", legacy)
		return
	}
	slog.Debug("Both config formats exist; using Ktav and ignoring legacy JSON",
		"kind", kind, "ktav", ktav, "json", legacy)
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
	if err != nil && !ktavEntryAbsentErr(err) {
		// Unreadable parent or I/O error: keep the Ktav spelling so the
		// stable reader surfaces the real error instead of silently
		// falling back to JSON.
		return ktav
	}
	if err == nil {
		if !info.IsDir() {
			if _, jsonErr := os.Stat(legacyJSON); jsonErr == nil {
				warnBothFormatsOnce("config", ktav, legacyJSON)
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
	return selectConfigSlot(filepath.Join(dataDir, cfgmodel.AppName+".json"))
}

// matchesLogicalSlot reports whether canonical names any serialization of
// the logical slot anchored at legacyJSON (the legacy JSON or its Ktav
// sibling). BOTH operands are normalized through legacySlotOf before the
// comparison: the anchor itself may already be a .ktav path (load.go
// selects globalDataPath through selectConfigSlot), so comparing a legacy
// spelling against a Ktav anchor must still match. Backup and
// transaction-record spellings are never write targets.
func matchesLogicalSlot(canonical, legacyJSON string) bool {
	if legacyJSON == "" {
		return false
	}
	return normalizeReloadPath(legacySlotOf(canonical)) == normalizeReloadPath(legacySlotOf(legacyJSON))
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
	if datadir.SystemConfigSlot() != "" {
		if err := checkBackupOnlySlot(datadir.SystemConfigSlot()); err != nil {
			return err
		}
	}
	for _, slot := range []string{datadir.GlobalConfig(), datadir.GlobalConfigData()} {
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
	abs := datadir.CanonicalConfigPath(workingDir)
	boundary := datadir.CanonicalConfigPath(datadir.ProjectBoundary(abs))
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
		for _, base := range configSlotBaseNames() {
			slots = append(slots, filepath.Join(dir, base+".json"))
		}
		if datadir.SameDir(dir, boundary) || filepath.Dir(dir) == dir {
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
	if datadir.SystemConfigSlot() != "" {
		add(datadir.SystemConfigSlot())
	}
	for _, slot := range []string{datadir.GlobalConfig(), datadir.GlobalConfigData()} {
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
	return err == nil || !ktavEntryAbsentErr(err)
}

// ktavEntryAbsentErr reports a stat failure that proves the entry cannot
// exist: not found, or a parent path component that is not a directory.
func ktavEntryAbsentErr(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// settingsDocumentFormat distinguishes the disk representations a settings
// document can use: the legacy JSON spelling and the Ktav spelling.
type settingsDocumentFormat int

const (
	settingsDocumentJSON settingsDocumentFormat = iota
	settingsDocumentKtav
)

// settingsDocument pairs one settings file's raw on-disk bytes with the
// JSON-shaped bytes every guard, merge, and mutation operates on. The raw
// fingerprint is bound to the RAW bytes as read; it must never be computed
// from (or compared against) the normalized JSON bytes.
type settingsDocument struct {
	path           string
	jsonBytes      []byte
	rawBytes       []byte
	rawFingerprint reloadFileFingerprint
	format         settingsDocumentFormat
}

// SettingsDocument pairs a settings file's raw on-disk bytes with the
// JSON-shaped bytes used by decoders and mutations.
type SettingsDocument struct {
	Path           string
	JSONBytes      []byte
	RawBytes       []byte
	RawFingerprint Fingerprint
	Format         int
}

// settingsDocumentFormatOf reports the disk format a path's extension names.
func settingsDocumentFormatOf(path string) settingsDocumentFormat {
	if isKtavConfigPath(path) {
		return settingsDocumentKtav
	}
	return settingsDocumentJSON
}

// decodeSettingsDocument converts raw bytes read from path into a
// settingsDocument. A decode failure is returned as-is: it must never be
// interpreted as a missing field, an absent sync_rev, or a nonexistent
// document, and the codec's error messages carry no document values.
func decodeSettingsDocument(path string, rawBytes []byte, rawFingerprint reloadFileFingerprint) (settingsDocument, error) {
	jsonBytes, err := decodeConfigDocument(path, rawBytes)
	if err != nil {
		return settingsDocument{}, err
	}
	return settingsDocument{
		path:           path,
		jsonBytes:      jsonBytes,
		rawBytes:       rawBytes,
		rawFingerprint: rawFingerprint,
		format:         settingsDocumentFormatOf(path),
	}, nil
}

// readSettingsDocument is THE single choke point for reading any settings or
// protection document. It takes a LOGICAL path in any spelling (legacy rush.json
// or a rush.ktav sibling, any case) and, BY CONSTRUCTION rather than per call
// site:
//
//  1. normalizes it to its logical LEGACY name (legacySlotOf);
//  2. selects the active slot (selectConfigSlot — the Ktav sibling wins);
//  3. fails closed BEFORE reading when the slot is backup-only
//     (checkBackupOnlySlot) — that error propagates as an ERROR and must
//     never be mapped to "no lock"/"unlocked" by any caller;
//  4. stable-reads the SELECTED file and decodes it (decodeConfigDocument);
//     decode errors propagate as errors.
//
// CONTRACT: an os.IsNotExist returned from this function means EXACTLY that
// neither the .ktav nor the .json spelling of the slot exists AND no backup
// exists (a backup-only slot already errored in step 3). Callers may treat
// such an error as "no document"; every other error is a real failure.
func readSettingsDocument(path string) (settingsDocument, error) {
	legacy := legacySlotOf(path)
	selected := selectConfigSlot(legacy)
	if err := checkBackupOnlySlot(legacy); err != nil {
		return settingsDocument{}, err
	}
	rawBytes, rawFingerprint, err := readStableConfigFile(selected)
	if err != nil {
		return settingsDocument{}, err
	}
	return decodeSettingsDocument(selected, rawBytes, rawFingerprint)
}

// encodeSettingsDocument converts JSON-shaped bytes back to the raw bytes a
// format-appropriate commit should write. JSON output is byte-for-byte the
// input, so .json targets behave exactly as before this layer existed. Ktav
// output is produced by the strict JSON-to-Ktav converter, which re-parses
// and refuses anything it cannot represent losslessly.
func encodeSettingsDocument(jsonBytes []byte, format settingsDocumentFormat) ([]byte, error) {
	if format == settingsDocumentJSON {
		return jsonBytes, nil
	}
	ktavBytes, err := ktavcodec.ConvertJSONToKtav(jsonBytes)
	if err != nil {
		return nil, fmt.Errorf("ktav conversion failed: %w", err)
	}
	return ktavBytes, nil
}

// rejectUnconvertedKtavWrite fails a write boundary that has not yet been
// converted to the settings-document layer when its target is a Ktav
// document. Wave B call sites (store_oauth, store_mcp) use this until their
// conversions land.
func rejectUnconvertedKtavWrite(path string) error {
	if isKtavConfigPath(filepath.Clean(path)) {
		return fmt.Errorf("%w: %s", errKtavWriteNotSupported, path)
	}
	return nil
}

// legacySlotOf returns the LEGACY JSON spelling of a logical config slot:
// a .ktav path maps to its .json sibling, any other path is returned
// unchanged. Mirrors ktavSiblingOf's construction inverted: same directory,
// same base name, .ktav replaced with .json.
//
// The extension strip is deliberately case-insensitive, NOT strings.TrimSuffix:
// isKtavConfigPath matches the extension with strings.EqualFold, so "rush.KTAV"
// is a Ktav path, but TrimSuffix (byte-case-sensitive) would leave it intact
// and produce the bogus slot "rush.KTAV.json". EqualFold-matched extensions
// have identical length, so cutting by length is safe. (The case-insensitive
// classification lives in isKtavConfigPath in ktav_discovery.go, which this
// lane does not own; this helper only keeps the strip consistent with it.)
func legacySlotOf(path string) string {
	if !isKtavConfigPath(path) {
		return path
	}
	return path[:len(path)-len(ktavConfigExt)] + ".json"
}

// configWriteLockAnchor returns the path the inter-process write sidecar
// anchors on: the LEGACY JSON spelling of a .ktav path, so a rush.ktav store
// contends on rush.json.lock — the same sidecar the pre-Ktav write path
// used. A non-Ktav path is returned unchanged, so a .json input anchors on
// itself.
func configWriteLockAnchor(path string) string {
	return legacySlotOf(path)
}

// readSlotOf resolves the physical READ path for a logical slot named by a
// legacy config path: rush.ktav when it is the selected file of the slot,
// otherwise the legacy name. It is the READ counterpart of writeTargetSlot:
// the same guard (empty and already-Ktav inputs pass through unchanged, so
// an already-selected .ktav path is never re-selected into "rush.ktav.ktav"),
// WITHOUT the creation redirect — a read source must observe the slot as it
// exists and never reserve the Ktav spelling for a slot that has neither
// file.
//
// Note: with the slot selection now built into readSettingsDocument, this
// helper is redundant for settings-protection callers but remains in use by
// the platform source helpers (settings_lock_sources*.go) and
// settings_models_integrity.go, which pass its result to path-sensitive
// logic (not just document reads).
func readSlotOf(path string) string {
	if path == "" || isKtavConfigPath(path) {
		return path
	}
	return selectConfigSlot(path)
}

// Fingerprint is the opaque snapshot token for one config document.
// ReadSettingsDocument reads and decodes the selected config slot.
func ReadSettingsDocument(path string) (SettingsDocument, error) {
	doc, err := readSettingsDocument(path)
	if err != nil {
		return SettingsDocument{}, err
	}
	return exportSettingsDocument(doc), nil
}

// DecodeSettingsDocument decodes raw bytes and binds their original fingerprint.
func DecodeSettingsDocument(path string, rawBytes []byte, fingerprint Fingerprint) (SettingsDocument, error) {
	doc, err := decodeSettingsDocument(path, rawBytes, importFingerprint(fingerprint))
	if err != nil {
		return SettingsDocument{}, err
	}
	return exportSettingsDocument(doc), nil
}

func exportSettingsDocument(doc settingsDocument) SettingsDocument {
	return SettingsDocument{
		Path:           doc.path,
		JSONBytes:      doc.jsonBytes,
		RawBytes:       doc.rawBytes,
		RawFingerprint: exportFingerprint(doc.rawFingerprint),
		Format:         int(doc.format),
	}
}

// EncodeSettingsDocument converts JSON-shaped bytes to the selected disk format.
func EncodeSettingsDocument(data []byte, format int) ([]byte, error) {
	return encodeSettingsDocument(data, settingsDocumentFormat(format))
}

// RejectUnconvertedKtavWrite rejects a direct write to a Ktav config file.
func RejectUnconvertedKtavWrite(path string) error { return rejectUnconvertedKtavWrite(path) }

// KtavSiblingOf returns the preferred Ktav spelling of a legacy JSON path.
func KtavSiblingOf(path string) string { return ktavSiblingOf(path) }

// BackupPathFor returns the retained backup spelling for a legacy JSON path.
func BackupPathFor(path string) string { return backupPathFor(path) }

// MigrationRecordPathFor returns the migration transaction record path.
func MigrationRecordPathFor(path string) string { return migrationRecordPathFor(path) }

// IsKtavConfigPath reports whether a path names a Ktav config document.
func IsKtavConfigPath(path string) bool { return isKtavConfigPath(path) }

// IsKtavBackupPath reports whether a path names a retained config backup.
func IsKtavBackupPath(path string) bool { return isKtavBackupPath(path) }

// IsKtavMigrationRecordPath reports whether a path names a migration record.
func IsKtavMigrationRecordPath(path string) bool { return isKtavMigrationRecordPath(path) }

// WarnBothFormatsOnce logs the both-formats condition once per source and kind.
func WarnBothFormatsOnce(kind, ktav, legacy string) { warnBothFormatsOnce(kind, ktav, legacy) }

// WarnMigrationDeferredOnce logs a deferred migration once per legacy source.
func WarnMigrationDeferredOnce(legacy, reason string) { warnMigrationDeferredOnce(legacy, reason) }

// SelectConfigSlot returns the selected physical path for a logical slot.
func SelectConfigSlot(path string) string { return selectConfigSlot(path) }

// DecodeConfigDocument converts Ktav bytes into JSON-shaped config bytes.
func DecodeConfigDocument(path string, data []byte) ([]byte, error) {
	return decodeConfigDocument(path, data)
}

// WorkspaceConfigPath selects the config spelling inside a data directory.
func WorkspaceConfigPath(path string) string { return workspaceConfigPath(path) }

// MatchesLogicalSlot reports whether two paths name the same logical slot.
func MatchesLogicalSlot(path, slot string) bool { return matchesLogicalSlot(path, slot) }

// CheckBackupOnlySlot rejects a config slot with only a retained backup.
func CheckBackupOnlySlot(path string) error { return checkBackupOnlySlot(path) }

// CheckAuthoritativeBackupOnly checks every authoritative config slot.
func CheckAuthoritativeBackupOnly(workingDir string) error {
	return checkAuthoritativeBackupOnly(workingDir)
}

// KtavMigrationPendingSlots returns legacy slots with incomplete transactions.
func KtavMigrationPendingSlots(workingDir string) []string {
	return ktavMigrationPendingSlots(workingDir)
}

// KtavEntryExists reports whether an entry exists or cannot be proven absent.
func KtavEntryExists(path string) bool { return ktavEntryExists(path) }

// KtavEntryAbsentErr reports whether an error proves an entry is absent.
func KtavEntryAbsentErr(err error) bool { return ktavEntryAbsentErr(err) }

// FileExists reports whether a non-directory file exists at path.
func FileExists(path string) bool { return fileExists(path) }

// ExpandConfigSlotSiblings appends the alternate spelling of every config slot.
func ExpandConfigSlotSiblings(paths []string) []string { return expandConfigSlotSiblings(paths) }

// LegacySlotOf returns the legacy JSON spelling of a logical config slot.
func LegacySlotOf(path string) string { return legacySlotOf(path) }

// ConfigWriteLockAnchor returns the legacy JSON path used for the config lock.
func ConfigWriteLockAnchor(path string) string { return configWriteLockAnchor(path) }

// ReadSlotOf resolves the physical read path for a logical config slot.
func ReadSlotOf(path string) string { return readSlotOf(path) }

// ErrKtavBackupOnly returns docread's single backup-only sentinel identity.
func ErrKtavBackupOnly() error { return errKtavBackupOnly }

// ErrKtavMigrationConflict returns docread's single migration-conflict sentinel identity.
func ErrKtavMigrationConflict() error { return errKtavMigrationConflict }

// ErrKtavMigrationRecord returns docread's single migration-record sentinel identity.
func ErrKtavMigrationRecord() error { return errKtavMigrationRecord }
