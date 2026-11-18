// Ktav slot discovery and format-aware settings documents.
package docread

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"

	"github.com/PHPCraftdream/rush/internal/config/catalog"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
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
	errOAuthJournalInvalid = errors.New("invalid OAuth journal")
)

// Only the predecessor refresh-token digest is persisted, never its secret.
type OAuthJournalRecord struct {
	PredecessorRefreshTokenSHA256 string       `json:"predecessor_refresh_token_sha256"`
	Token                         *oauth.Token `json:"token"`
	BurnedRefreshTokenSHA256      []string     `json:"burned_refresh_token_sha256,omitempty"`
}

func ValidOAuthJournal(record *OAuthJournalRecord) bool {
	if record == nil || record.Token == nil || record.Token.AccessToken == "" || record.Token.RefreshToken == "" || record.Token.ExpiresAt <= 0 {
		return false
	}
	if record.PredecessorRefreshTokenSHA256 != "" {
		digest, err := hex.DecodeString(record.PredecessorRefreshTokenSHA256)
		if err != nil || len(digest) != sha256.Size {
			return false
		}
	}
	for _, burned := range record.BurnedRefreshTokenSHA256 {
		digest, err := hex.DecodeString(burned)
		if err != nil || len(digest) != sha256.Size {
			return false
		}
	}
	return true
}

// oauthJournalDigest returns the documented 8-byte hex suffix for a
// provider's journal and sidecar names.
func OAuthJournalDigest(providerID string) string {
	digest := sha256.Sum256([]byte(providerID))
	return hex.EncodeToString(digest[:8])
}

// oauthJournalLegacyPath returns the LEGACY JSON spelling of a provider's
// pending journal: rush.json.oauth-pending-<hash>.json. It anchors on the
// LEGACY slot name even for a rush.ktav config, so a migrated config still
// discovers journals written before migration.
func OAuthJournalLegacyPath(configPath, providerID string) string {
	return NormalizeReloadPath(legacySlotOf(configPath)) + ".oauth-pending-" + OAuthJournalDigest(providerID) + ".json"
}

// oauthJournalKtavPath returns the Ktav spelling of a provider's pending
// journal: rush.ktav.oauth-pending-<hash>.ktav, anchored on the ACTIVE
// Ktav sibling of the legacy config slot. (KTAV-4: automatic conversion
// of existing legacy JSON journals is deferred.)
func OAuthJournalKtavPath(configPath, providerID string) string {
	return NormalizeReloadPath(ktavSiblingOf(legacySlotOf(configPath))) + ".oauth-pending-" + OAuthJournalDigest(providerID) + ".ktav"
}

func ErrOAuthJournalInvalid() error { return errOAuthJournalInvalid }

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

// WarnKtavArtifactConflictOnce logs an artifact migration that stopped on a
// conflict beside an EXISTING destination Ktav: the existing Ktav generation
// stays active and the JSON copy is left untouched. It keys off the shared
// ktavBothFormatsWarned map (distinct prefix) so tests can reset it, once
// per destination, paths only.
func WarnKtavArtifactConflictOnce(ktav, legacy, reason string) {
	if _, dup := ktavBothFormatsWarned.LoadOrStore("conflict:"+ktav, struct{}{}); dup {
		return
	}
	slog.Warn("Ktav artifact migration stopped; an existing Ktav generation stays active and the JSON copy is left untouched",
		"ktav", ktav, "json", legacy, "reason", reason)
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
	return NormalizeReloadPath(legacySlotOf(canonical)) == NormalizeReloadPath(legacySlotOf(legacyJSON))
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

// SettingsDocumentFormat distinguishes the disk representations a settings
// document can use: the legacy JSON spelling and the Ktav spelling.
type SettingsDocumentFormat int

const (
	settingsDocumentJSON SettingsDocumentFormat = iota
	settingsDocumentKtav
)

// SettingsDocument pairs one settings file's raw on-disk bytes with the
// JSON-shaped bytes used by decoders and mutations.
type SettingsDocument struct {
	Path           string
	JSONBytes      []byte
	RawFingerprint Fingerprint
	Format         SettingsDocumentFormat
}

// SettingsDocumentFormatOf reports the disk format a path's extension names.
func SettingsDocumentFormatOf(path string) SettingsDocumentFormat {
	if isKtavConfigPath(path) {
		return settingsDocumentKtav
	}
	return settingsDocumentJSON
}

// DecodeSettingsDocument decodes raw bytes while retaining their fingerprint.
func DecodeSettingsDocument(path string, rawBytes []byte, rawFingerprint Fingerprint) (SettingsDocument, error) {
	jsonBytes, err := DecodeConfigDocument(path, rawBytes)
	if err != nil {
		return SettingsDocument{}, err
	}
	return SettingsDocument{
		Path: path, JSONBytes: jsonBytes,
		RawFingerprint: rawFingerprint, Format: SettingsDocumentFormatOf(path),
	}, nil
}

// ReadSettingsDocument reads and decodes the selected config slot.
func ReadSettingsDocument(path string) (SettingsDocument, error) {
	legacy := LegacySlotOf(path)
	selected := SelectConfigSlot(legacy)
	if err := CheckBackupOnlySlot(legacy); err != nil {
		return SettingsDocument{}, err
	}
	rawBytes, rawFingerprint, err := ReadStableConfigFile(selected)
	if err != nil {
		return SettingsDocument{}, err
	}
	return DecodeSettingsDocument(selected, rawBytes, rawFingerprint)
}

// EncodeSettingsDocument converts JSON-shaped bytes to the selected disk format.
func EncodeSettingsDocument(data []byte, format SettingsDocumentFormat) ([]byte, error) {
	if format == settingsDocumentJSON {
		return data, nil
	}
	ktavBytes, err := ktavcodec.ConvertJSONToKtav(data)
	if err != nil {
		return nil, fmt.Errorf("ktav conversion failed: %w", err)
	}
	return ktavBytes, nil
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

// CheckAuthoritativeBackupOnly checks every authoritative config location.
func CheckAuthoritativeBackupOnly(workingDir string) error {
	return checkAuthoritativeBackupOnly(workingDir)
}

// CheckBackupOnlySlot fails closed for a backup-only config slot.
func CheckBackupOnlySlot(path string) error { return checkBackupOnlySlot(path) }

// KtavSiblingOf returns the preferred Ktav spelling of a legacy JSON path.
func KtavSiblingOf(path string) string { return ktavSiblingOf(path) }

// BackupPathFor returns the retained backup spelling for a legacy config backup.
func BackupPathFor(path string) string { return backupPathFor(path) }

// MigrationRecordPathFor returns the migration transaction record path.
func MigrationRecordPathFor(path string) string { return migrationRecordPathFor(path) }

// IsKtavConfigPath reports whether path names a Ktav config document.
func IsKtavConfigPath(path string) bool { return isKtavConfigPath(path) }

// IsKtavBackupPath reports whether path names a retained backup.
func IsKtavBackupPath(path string) bool { return isKtavBackupPath(path) }

// IsKtavMigrationRecordPath reports whether path names a migration record.
func IsKtavMigrationRecordPath(path string) bool { return isKtavMigrationRecordPath(path) }

// WarnBothFormatsOnce logs the both-formats condition once per source and kind.
func WarnBothFormatsOnce(kind, ktav, legacy string) { warnBothFormatsOnce(kind, ktav, legacy) }

// WarnMigrationDeferredOnce logs a deferred migration once per legacy source.
func WarnMigrationDeferredOnce(legacy, reason string) { warnMigrationDeferredOnce(legacy, reason) }

// SelectConfigSlot returns the selected physical path for a logical slot.
func SelectConfigSlot(path string) string { return selectConfigSlot(path) }

// DecodeConfigDocument decodes a selected document into JSON-shaped bytes.
func DecodeConfigDocument(path string, data []byte) ([]byte, error) {
	return decodeConfigDocument(path, data)
}

// WorkspaceConfigPath resolves the preferred config spelling inside a data directory.
func WorkspaceConfigPath(path string) string { return workspaceConfigPath(path) }

// MatchesLogicalSlot reports whether two paths refer to the same logical slot.
func MatchesLogicalSlot(path, slot string) bool { return matchesLogicalSlot(path, slot) }

// KtavMigrationPendingSlots reports slots with incomplete migration records.
func KtavMigrationPendingSlots(workingDir string) []string {
	return ktavMigrationPendingSlots(workingDir)
}

// KtavEntryExists reports whether a path exists, including non-regular entries.
func KtavEntryExists(path string) bool { return ktavEntryExists(path) }

// KtavEntryAbsentErr reports whether an error proves that the entry cannot exist.
func KtavEntryAbsentErr(err error) bool { return ktavEntryAbsentErr(err) }

// ExpandConfigSlotSiblings adds alternate spellings for logical config slots.
func ExpandConfigSlotSiblings(paths []string) []string { return expandConfigSlotSiblings(paths) }

// LegacySlotOf returns the legacy JSON spelling of a logical config slot.
func LegacySlotOf(path string) string { return legacySlotOf(path) }

// ConfigWriteLockAnchor returns the legacy slot spelling used by write locks.
func ConfigWriteLockAnchor(path string) string { return legacySlotOf(path) }

// ReadSlotOf resolves the physical read path for a logical config slot.
func ReadSlotOf(path string) string { return readSlotOf(path) }

// FileExists reports whether a non-directory file exists at path.
func FileExists(path string) bool { return fileExists(path) }

// ErrKtavBackupOnly returns docread's single backup-only sentinel identity.
func ErrKtavBackupOnly() error { return errKtavBackupOnly }

// ErrKtavMigrationConflict returns docread's single migration-conflict sentinel identity.
func ErrKtavMigrationConflict() error { return errKtavMigrationConflict }

// ErrKtavMigrationRecord returns docread's single migration-record sentinel identity.
func ErrKtavMigrationRecord() error { return errKtavMigrationRecord }

// errOAuthJournalRead reports a journal that exists but cannot be read from
// disk; readers treat it as transient and retry with backoff.
var errOAuthJournalRead = errors.New("OAuth journal cannot be read")

// ErrOAuthJournalRead returns docread's single journal-read sentinel identity.
func ErrOAuthJournalRead() error { return errOAuthJournalRead }

// OAuthRefreshLockPath mirrors config.oauthRefreshLockPath: the SAME
// legacy-anchored refresh sidecar lock the recovery worker uses.
func OAuthRefreshLockPath(path, providerID string) string {
	digest := sha256.Sum256([]byte(providerID))
	return fmt.Sprintf("%s.oauth-refresh-%x.lock", NormalizeReloadPath(ConfigWriteLockAnchor(path)), digest[:8])
}

// ParseOAuthJournal decodes and validates raw journal bytes read from path.
// Unreadable or corrupt journals are kept: any decode or validation failure
// reports ErrOAuthJournalInvalid.
func ParseOAuthJournal(path string, data []byte) (*OAuthJournalRecord, error) {
	jsonBytes, decodeErr := DecodeConfigDocument(path, data)
	if decodeErr != nil {
		return nil, ErrOAuthJournalInvalid()
	}
	var record OAuthJournalRecord
	if json.Unmarshal(jsonBytes, &record) != nil || !ValidOAuthJournal(&record) {
		return nil, ErrOAuthJournalInvalid()
	}
	return &record, nil
}

// ReadOAuthJournal mirrors config.readOAuthJournal: missing journals are
// normal; unreadable or corrupt journals are kept.
func ReadOAuthJournal(path string) (*OAuthJournalRecord, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrOAuthJournalRead()
	}
	return ParseOAuthJournal(path, data)
}

// OAuthProviderInfo carries only the fields the artifact migrator needs.
type OAuthProviderInfo struct {
	ID       string
	Rotating bool
}

// OAuthProviderDescriptors mirrors config.oauthProviderDescriptors. Keep in
// sync: copilot reuses its credential; Hyper's rotation contract is unknown —
// treated as non-rotating.
func OAuthProviderDescriptors() []OAuthProviderInfo {
	return []OAuthProviderInfo{
		{ID: "openai-codex", Rotating: true},
		{ID: string(catwalk.InferenceProviderCopilot), Rotating: false},
		{ID: catalog.HyperName, Rotating: false},
	}
}
