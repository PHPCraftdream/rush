// Ktav migration service: converts an active legacy JSON config into a
// Ktav file under a single lock-protected, crash-recoverable transaction.
package slotmigrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/cfghooks"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filelock"
	"github.com/PHPCraftdream/rush/internal/kit/text/ktavcodec"
)

// ktavOAuthJournalReadBackoffs bound the retries on journal I/O; they mirror
// config's oauthJournalReadBackoffs.
var ktavOAuthJournalReadBackoffs = []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}

// readOAuthJournalWithRetry mirrors config.readOAuthJournalWithRetry: only
// journal I/O receives bounded, context-cancelable retries.
func readOAuthJournalWithRetry(ctx context.Context, path string) (*docread.OAuthJournalRecord, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := docread.ReadOAuthJournal(path)
		if !errors.Is(err, docread.ErrOAuthJournalRead()) || attempt >= len(ktavOAuthJournalReadBackoffs) {
			return record, err
		}
		wait := time.NewTimer(ktavOAuthJournalReadBackoffs[attempt])
		select {
		case <-ctx.Done():
			wait.Stop()
			return nil, ctx.Err()
		case <-wait.C:
		}
	}
}

// acquireConfigFileLock mirrors cfgcommit's hook-overridable lock acquisition.
func acquireConfigFileLock(ctx context.Context, path string) (*filelock.FileLock, error) {
	if hook := cfghooks.Snapshot().AcquireConfigLock; hook != nil {
		return hook(ctx, path)
	}
	return filelock.AcquireFileLockContext(ctx, path)
}

// releaseConfigFileLock mirrors cfgcommit's hook-overridable lock release.
func releaseConfigFileLock(path string, lock *filelock.FileLock) error {
	if hook := cfghooks.Snapshot().ReleaseConfigLock; hook != nil {
		return hook(path, lock)
	}
	return lock.Release()
}

var (
	// Automatic rush.json -> rush.ktav migration is ACTIVE: every Rush-owned
	// reader and writer is Ktav-capable. Tests that need the legacy JSON
	// behavior set this to false explicitly and restore it.
	ktavAutoMigrate     = true
	ktavMigrationBudget = 15 * time.Second
)

// KtavMigrationBudget returns the shared ktav migration budget (production
// accessor; GetKtavMigrationBudgetForTest remains for tests).
func KtavMigrationBudget() time.Duration { return ktavMigrationBudget }

const KtavMigrationRecordVersion = 1

// appName is a byte-identical local copy of the root package's appName
// (cfgmodel_locals.go); slotmigrate sits below the root in the graph.
const appName = "rush"

var (
	errKtavMigrationCrashInjected = errors.New("ktav migration: crash injected at test boundary")
	ktavMigrationStepHook         func(step string) error
)

func runKtavMigrationStep(step string) error {
	if ktavMigrationStepHook != nil {
		return ktavMigrationStepHook(step)
	}
	return nil
}

func ktavMigrationCheckContext(ctx context.Context, step string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("ktav migration %s: %w", step, err)
	}
	return runKtavMigrationStep(step)
}

func ktavMigrationCommitHook(ctx context.Context, stage string) func(string) error {
	return func(step string) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("ktav migration %s: %w", step, err)
		}
		if step == "after-rename" && stage == "publish" {
			if err := runKtavMigrationStep("after-ktav-rename"); err != nil {
				return err
			}
			return nil
		}
		return nil
	}
}

type KtavMigrationRecord struct {
	Version      int    `json:"version"`
	Source       string `json:"source"`
	Destination  string `json:"destination"`
	Backup       string `json:"backup"`
	SourceDigest string `json:"source_digest"`
	StagedDigest string `json:"staged_digest"`
	SourceID     string `json:"source_id"`
	ParentID     string `json:"parent_id"`
	Symlink      bool   `json:"symlink,omitempty"`
}

type ktavMigrationEntries struct {
	jsonExists   bool
	ktavExists   bool
	backupExists bool
	recordExists bool
}

func inspectKtavMigrationEntries(legacy string) ktavMigrationEntries {
	return ktavMigrationEntries{
		jsonExists:   docread.KtavEntryExists(legacy),
		ktavExists:   docread.KtavEntryExists(docread.KtavSiblingOf(legacy)),
		backupExists: docread.KtavEntryExists(docread.BackupPathFor(legacy)),
		recordExists: docread.KtavEntryExists(docread.MigrationRecordPathFor(legacy)),
	}
}

func migrateLegacyConfigFile(ctx context.Context, legacy, workingDir string) error {
	if legacy == "" {
		return fmt.Errorf("ktav migration: empty legacy path")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ktavMigrationBudget)
		defer cancel()
	}
	return runKtavMigrationState(ctx, legacy, workingDir)
}

// MigrateLegacyConfigFile migrates one legacy config file to the working
// directory. Exported wrapper over migrateLegacyConfigFile.
func MigrateLegacyConfigFile(ctx context.Context, legacy, workingDir string) error {
	return migrateLegacyConfigFile(ctx, legacy, workingDir)
}

func runKtavMigrationState(ctx context.Context, legacy, workingDir string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ktavMigrationBudget)
		defer cancel()
	}
	lockPath := legacy + ".lock"
	lock, err := acquireConfigFileLock(ctx, lockPath)
	if err != nil {
		return fmt.Errorf("ktav migration: failed to lock %s: %w", filepath.Base(lockPath), err)
	}
	defer func() { _ = releaseConfigFileLock(lockPath, lock) }()
	if err := runKtavMigrationStep("after-lock"); err != nil {
		return err
	}
	return runKtavMigrationStateLocked(ctx, legacy, workingDir)
}

// RunKtavMigrationStateLocked runs the state-locked ktav migration for callers
// that already hold the sidecar file lock (root production wrapper).
func RunKtavMigrationStateLocked(ctx context.Context, legacy, workingDir string) error {
	return runKtavMigrationStateLocked(ctx, legacy, workingDir)
}

func runKtavMigrationStateLocked(ctx context.Context, legacy, workingDir string) error {
	if err := runKtavMigrationStep("before-state-inspection"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("ktav migration: budget exhausted: %w", err)
	}
	backup := docread.BackupPathFor(legacy)
	recordPath := docread.MigrationRecordPathFor(legacy)
	state := inspectKtavMigrationEntries(legacy)
	ktavPath := docread.KtavSiblingOf(legacy)
	handled, err := runKtavSymlinkMigrationState(ctx, legacy, recordPath, workingDir, state)
	if handled {
		return err
	}
	switch {
	case !state.ktavExists && !state.jsonExists && !state.recordExists && !state.backupExists:
		return nil
	case !state.ktavExists && !state.jsonExists && state.backupExists && !state.recordExists:
		return fmt.Errorf("%w: %s exists without %s or %s", docread.ErrKtavBackupOnly(), filepath.Base(backup), filepath.Base(legacy), filepath.Base(ktavPath))
	case state.ktavExists && state.backupExists && !state.jsonExists:
		if !state.recordExists {
			return nil
		}
		record, err := loadKtavMigrationRecord(recordPath, legacy, ktavPath, workingDir)
		if err != nil {
			return err
		}
		owner, enforce, err := OwnerPolicyForMigration(legacy, workingDir)
		if err != nil {
			return err
		}
		if err := verifyKtavGenerations(record, legacy, ktavPath, owner, enforce, state); err != nil {
			return err
		}
		if err := cfgcommit.SyncConfigParent(filepath.Dir(recordPath)); err != nil {
			return err
		}
		if err := runKtavMigrationStep("after-archive"); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return removeKtavMigrationRecord(recordPath)
	case state.ktavExists && !state.jsonExists && !state.backupExists:
		if !state.recordExists {
			return nil
		}
		if _, err := loadKtavMigrationRecord(recordPath, legacy, ktavPath, workingDir); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s and its migration record exist but source and backup are gone", docread.ErrKtavMigrationConflict(), filepath.Base(ktavPath))
	case state.jsonExists && state.backupExists && !KtavKeepsLegacy(legacy):
		return fmt.Errorf("%w: unexpected backup %s beside active %s; preserve every file and resolve manually", docread.ErrKtavMigrationConflict(), filepath.Base(backup), filepath.Base(legacy))
	case state.ktavExists && state.jsonExists:
		if !state.recordExists {
			if KtavKeepsLegacy(legacy) {
				// The migration keeps the legacy JSON: Ktav is the active
				// generation and the JSON stays for older binaries.
				return nil
			}
			return fmt.Errorf("%w: Ktav and JSON coexist without an authenticated migration record", docread.ErrKtavMigrationConflict())
		}
		record, err := loadKtavMigrationRecord(recordPath, legacy, ktavPath, workingDir)
		if err != nil {
			return err
		}
		owner, enforce, err := OwnerPolicyForMigration(legacy, workingDir)
		if err != nil {
			return err
		}
		if err := verifyKtavGenerations(record, legacy, ktavPath, owner, enforce, state); err != nil {
			return err
		}
		return finishKtavArchival(ctx, legacy, ktavPath, backup, recordPath, record, workingDir)
	}
	return runKtavMigrationTransaction(ctx, legacy, backup, recordPath, workingDir)
}

func runKtavMigrationTransaction(ctx context.Context, legacy, backup, recordPath, workingDir string) error {
	state := inspectKtavMigrationEntries(legacy)
	var priorRecord *KtavMigrationRecord
	if state.recordExists {
		var err error
		priorRecord, err = loadKtavMigrationRecord(recordPath, legacy, docread.KtavSiblingOf(legacy), workingDir)
		if err != nil {
			return err
		}
	}
	if state.backupExists && !KtavKeepsLegacy(legacy) {
		return fmt.Errorf("%w: backup %s already exists", docread.ErrKtavMigrationConflict(), filepath.Base(backup))
	}
	owner, enforce, err := OwnerPolicyForMigration(legacy, workingDir)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source, sourceFP, err := docread.ReadStableConfigFileOwned(legacy, owner, enforce)
	if err != nil {
		return fmt.Errorf("ktav migration: failed to read %s: %w", legacy, err)
	}
	if sourceFP.Nlink != 1 {
		return fileid.ErrConfigHardLink
	}
	if priorRecord != nil && (!validKtavMigrationDigest(priorRecord.SourceDigest) || fmt.Sprintf("%v", sourceFP.Identity) != priorRecord.SourceID || fmt.Sprintf("%v", sourceFP.ParentIdentity) != priorRecord.ParentID || hex.EncodeToString(sourceFP.Digest[:]) != priorRecord.SourceDigest) {
		return fmt.Errorf("%w: existing record does not match source %s", docread.ErrKtavMigrationConflict(), filepath.Base(legacy))
	}
	if err := ktavMigrationCheckContext(ctx, "after-read"); err != nil {
		return err
	}
	if err := ktavMigrationCheckContext(ctx, "before-convert"); err != nil {
		return err
	}
	ktavBytes, err := ktavcodec.ConvertJSONToKtav(source)
	if err != nil {
		return fmt.Errorf("ktav conversion failed for %s: %w", filepath.Base(legacy), err)
	}
	ktavBytes, err = ApplyKtavMigrationModelsIntegrity(source, ktavBytes)
	if err != nil {
		return err
	}
	if err := ktavMigrationCheckContext(ctx, "after-convert"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stagedDigest := sha256.Sum256(ktavBytes)
	record := KtavMigrationRecord{
		Version:      KtavMigrationRecordVersion,
		Source:       filepath.Base(legacy),
		Destination:  filepath.Base(docread.KtavSiblingOf(legacy)),
		Backup:       filepath.Base(backup),
		SourceDigest: hex.EncodeToString(sourceFP.Digest[:]),
		StagedDigest: hex.EncodeToString(stagedDigest[:]),
		SourceID:     fmt.Sprintf("%v", sourceFP.Identity),
		ParentID:     fmt.Sprintf("%v", sourceFP.ParentIdentity),
	}
	if priorRecord != nil {
		if fmt.Sprintf("%v", sourceFP.Identity) != priorRecord.SourceID || fmt.Sprintf("%v", sourceFP.ParentIdentity) != priorRecord.ParentID || hex.EncodeToString(sourceFP.Digest[:]) != priorRecord.SourceDigest || hex.EncodeToString(stagedDigest[:]) != priorRecord.StagedDigest {
			return fmt.Errorf("%w: existing record does not match regenerated transaction", docread.ErrKtavMigrationConflict())
		}
		record = *priorRecord
	}
	recordBytes, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("ktav migration: failed to encode record: %w", err)
	}
	ktavPath := docread.KtavSiblingOf(legacy)
	_, expected, err := docread.ReadStableConfigFileOwned(ktavPath, owner, enforce)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("ktav migration: failed to inspect %s: %w", ktavPath, err)
	}
	if expected.Exists {
		return fmt.Errorf("%w: %s appeared during migration", docread.ErrKtavMigrationConflict(), filepath.Base(ktavPath))
	}
	commitHooks := ktavMigrationCommitHook(ctx, "publish")
	publishHook := func(step string) error {
		if err := commitHooks(step); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if step == "before-staging" {
			return runKtavMigrationStep("before-staging")
		}
		if step == "during-staging" {
			return runKtavMigrationStep("during-staging")
		}
		if step == "after-staging" && priorRecord == nil {
			if err := ktavMigrationCheckContext(ctx, "before-record"); err != nil {
				return err
			}
			absent := docread.Fingerprint{
				Discovery:       docread.ConfigDiscoveryFingerprint(recordPath),
				ParentDiscovery: docread.ConfigDiscoveryFingerprint(filepath.Dir(recordPath)),
				ParentIdentity:  fileid.ConfigParentIdentity(recordPath),
				AliasChain:      docread.ConfigAliasChainFingerprint(recordPath),
			}
			if !absent.ParentIdentity.Valid {
				return fmt.Errorf("ktav migration: cannot pin record parent %s", filepath.Dir(recordPath))
			}
			if err := ktavMigrationCheckContext(ctx, "record-commit"); err != nil {
				return err
			}
			if _, err := cfgcommit.CommitConfigFile(recordPath, recordPath, append(recordBytes, '\n'), 0o600, absent, owner, enforce); err != nil {
				return fmt.Errorf("ktav migration: failed to persist record %s: %w", filepath.Base(recordPath), err)
			}
			if err := ktavMigrationCheckContext(ctx, "after-record"); err != nil {
				return err
			}
			return nil
		}
		return nil
	}
	if err := ktavMigrationCheckContext(ctx, "before-ktav-rename"); err != nil {
		return err
	}
	if _, err := cfgcommit.CommitConfigFile(ktavPath, ktavPath, ktavBytes, 0o600, expected, owner, enforce, publishHook); err != nil {
		return fmt.Errorf("ktav migration: failed to publish %s: %w", ktavPath, err)
	}
	if err := ktavMigrationCheckContext(ctx, "after-publish"); err != nil {
		return err
	}
	return finishKtavArchival(ctx, legacy, docread.KtavSiblingOf(legacy), backup, recordPath, &record, workingDir)
}

func finishKtavArchival(ctx context.Context, legacy, dest, backup, recordPath string, record *KtavMigrationRecord, workingDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, enforce, err := OwnerPolicyForMigration(legacy, workingDir)
	if err != nil {
		return err
	}
	if !validKtavMigrationDigest(record.SourceDigest) {
		return fmt.Errorf("%w: migration record source digest is malformed", docread.ErrKtavMigrationRecord())
	}
	if err := verifyKtavGenerations(record, legacy, dest, owner, enforce, inspectKtavMigrationEntries(legacy)); err != nil {
		return err
	}
	current, currentFP, err := docread.ReadStableConfigFileOwned(legacy, owner, enforce)
	if err != nil {
		return fmt.Errorf("ktav migration: failed to re-read %s: %w", legacy, err)
	}
	if err := runKtavMigrationStep("before-archive"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var want [sha256.Size]byte
	if _, err := hex.Decode(want[:], []byte(record.SourceDigest)); err != nil || currentFP.Digest != want || !docread.SameBytesFingerprint(current, want) || fmt.Sprintf("%v", currentFP.Identity) != record.SourceID || fmt.Sprintf("%v", currentFP.ParentIdentity) != record.ParentID {
		return fmt.Errorf("%w: %s changed after Ktav publication; the JSON stays active", docread.ErrKtavMigrationConflict(), filepath.Base(legacy))
	}
	if KtavKeepsLegacy(legacy) {
		// Kept in place (not archived) so older binaries that only read
		// JSON keep working; Ktav is the active generation.
		slog.Info("Ktav migration: legacy JSON kept beside the new Ktav generation", "json", legacy, "ktav", dest)
	} else if err := archiveKtavBackup(ctx, legacy, backup, currentFP, owner, enforce); err != nil {
		if errors.Is(err, docread.ErrKtavMigrationConflict()) {
			return err
		}
		return fmt.Errorf("ktav migration: failed to archive %s: %w", legacy, err)
	}
	if err := runKtavMigrationStep("after-archive"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return removeKtavMigrationRecord(recordPath)
}

func loadKtavMigrationRecord(recordPath, legacy, dest, workingDir string) (*KtavMigrationRecord, error) {
	owner, enforce, err := OwnerPolicyForMigration(legacy, workingDir)
	if err != nil {
		return nil, err
	}
	recordBytes, recordFP, err := docread.ReadStableConfigFileOwned(recordPath, owner, enforce)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: record %s vanished during recovery", docread.ErrKtavMigrationRecord(), filepath.Base(recordPath))
		}
		return nil, fmt.Errorf("%w: cannot read migration record %s: %v", docread.ErrKtavMigrationRecord(), filepath.Base(recordPath), err)
	}
	var record KtavMigrationRecord
	if err := json.Unmarshal(recordBytes, &record); err != nil {
		return nil, fmt.Errorf("%w: unreadable migration record %s", docread.ErrKtavMigrationRecord(), filepath.Base(recordPath))
	}
	// On Windows configParentIdentity resolves a symlinked leaf through
	// physicalWindowsConfigPath, so for a Symlink-marked record the legacy
	// path's parent anchor is the link TARGET's directory, not the directory
	// holding the link. Symlink records are pinned to the LINK directory by
	// the recordPath anchor above; the legacy-parent clause applies only to
	// regular (non-symlink) records.
	legacyParentAnchored := recordFP.ParentIdentity == fileid.ConfigParentIdentity(legacy)
	if record.Version != KtavMigrationRecordVersion || record.Source != filepath.Base(legacy) || record.Destination != filepath.Base(dest) || record.Backup != filepath.Base(docread.BackupPathFor(legacy)) || record.SourceID == "" || record.ParentID == "" || !validKtavMigrationDigest(record.SourceDigest) || !validKtavMigrationDigest(record.StagedDigest) || recordFP.ParentIdentity != fileid.ConfigParentIdentity(recordPath) || !recordFP.Identity.Valid || (!record.Symlink && !legacyParentAnchored) {
		return nil, fmt.Errorf("%w: migration record %s is malformed or bound to a different slot", docread.ErrKtavMigrationRecord(), filepath.Base(recordPath))
	}
	return &record, nil
}

func validKtavMigrationDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func verifyKtavGenerations(record *KtavMigrationRecord, legacy, dest string, owner int, enforceOwner bool, state ktavMigrationEntries) error {
	if !validKtavMigrationDigest(record.StagedDigest) || !validKtavMigrationDigest(record.SourceDigest) {
		return fmt.Errorf("%w: migration record digest is malformed", docread.ErrKtavMigrationRecord())
	}
	var staged [sha256.Size]byte
	if _, err := hex.Decode(staged[:], []byte(record.StagedDigest)); err != nil {
		return fmt.Errorf("%w: staged digest is malformed", docread.ErrKtavMigrationRecord())
	}
	ktavData, ktavFP, err := docread.ReadStableConfigFileOwned(dest, owner, enforceOwner)
	if err != nil {
		return fmt.Errorf("%w: cannot verify published Ktav %s: %v", docread.ErrKtavMigrationRecord(), filepath.Base(dest), err)
	}
	if ktavFP.Digest != staged || !docread.SameBytesFingerprint(ktavData, staged) {
		return fmt.Errorf("%w: migration record does not match published %s", docread.ErrKtavMigrationRecord(), filepath.Base(dest))
	}
	if state.jsonExists {
		data, fp, err := docread.ReadStableConfigFileOwned(legacy, owner, enforceOwner)
		if err != nil {
			return fmt.Errorf("%w: cannot verify source %s: %v", docread.ErrKtavMigrationRecord(), filepath.Base(legacy), err)
		}
		var want [sha256.Size]byte
		if _, err := hex.Decode(want[:], []byte(record.SourceDigest)); err != nil || fp.Digest != want || !docread.SameBytesFingerprint(data, want) || fmt.Sprintf("%v", fp.Identity) != record.SourceID || fmt.Sprintf("%v", fp.ParentIdentity) != record.ParentID {
			return fmt.Errorf("%w: migration record does not match source %s", docread.ErrKtavMigrationConflict(), filepath.Base(legacy))
		}
	}
	if state.backupExists && !state.jsonExists {
		data, fp, err := docread.ReadStableConfigFileOwned(docread.BackupPathFor(legacy), owner, enforceOwner)
		if err != nil {
			return fmt.Errorf("%w: cannot verify backup %s: %v", docread.ErrKtavMigrationRecord(), filepath.Base(docread.BackupPathFor(legacy)), err)
		}
		var want [sha256.Size]byte
		if _, err := hex.Decode(want[:], []byte(record.SourceDigest)); err != nil || fp.Digest != want || !docread.SameBytesFingerprint(data, want) || fmt.Sprintf("%v", fp.Identity) != record.SourceID || fmt.Sprintf("%v", fp.ParentIdentity) != record.ParentID {
			return fmt.Errorf("%w: migration record does not match backup %s", docread.ErrKtavMigrationRecord(), filepath.Base(docread.BackupPathFor(legacy)))
		}
	}
	return nil
}

// KtavKeepsLegacy reports whether migrating legacy leaves the JSON in place.
// Configs and caches stay for older binaries that only read JSON. A pending
// OAuth journal is an unfinished token transaction, so its JSON copy is still
// archived: replaying a stale duplicate could overwrite rotated tokens.
func KtavKeepsLegacy(legacy string) bool {
	return !strings.Contains(filepath.Base(legacy), ".oauth-pending-")
}

func removeKtavMigrationRecord(recordPath string) error {
	if err := os.Remove(recordPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("ktav migration: failed to remove record %s: %w", recordPath, err)
	}
	return cfgcommit.SyncConfigParent(filepath.Dir(recordPath))
}

func OwnerPolicyForMigration(path, workingDir string) (int, bool, error) {
	canonical := docread.NormalizeReloadPath(path)
	if datadir.SystemConfigSlot() != "" && docread.MatchesLogicalSlot(canonical, datadir.SystemConfigSlot()) || docread.MatchesLogicalSlot(canonical, datadir.GlobalConfig()) || docread.MatchesLogicalSlot(canonical, datadir.GlobalConfigData()) {
		return docread.ConfigOwnerPolicyForPath(path, workingDir)
	}
	projectDir := workingDir
	if projectDir == "" {
		projectDir = filepath.Dir(path)
	}
	return docread.ConfigOwnerPolicyForPath(path, projectDir)
}

func ktavMigrateStartupSlots(ctx context.Context, workingDir string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ktavMigrationBudget)
		defer cancel()
	}
	for _, legacy := range docread.KtavMigrationPendingSlots(workingDir) {
		if err := runKtavMigrationState(ctx, legacy, workingDir); err != nil {
			return fmt.Errorf("%w: cannot recover pending transaction for %s: %v", docread.ErrKtavMigrationRecord(), filepath.Base(legacy), err)
		}
	}
	if !ktavAutoMigrate {
		return nil
	}
	for _, path := range docread.LookupConfigCandidates(workingDir) {
		if path == "" || docread.IsKtavConfigPath(path) || docread.IsKtavBackupPath(path) || docread.IsKtavMigrationRecordPath(path) {
			continue
		}
		if docread.FileExists(docread.KtavSiblingOf(path)) {
			continue
		}
		if !docread.FileExists(path) {
			continue
		}
		if err := runKtavMigrationState(ctx, path, workingDir); err != nil {
			if errors.Is(err, docread.ErrKtavMigrationConflict()) || errors.Is(err, docread.ErrKtavBackupOnly()) || errors.Is(err, docread.ErrKtavMigrationRecord()) {
				return err
			}
			if docread.KtavEntryExists(docread.MigrationRecordPathFor(path)) || docread.KtavEntryExists(docread.KtavSiblingOf(path)) {
				return fmt.Errorf("%w: cannot continue migration for %s: %v", docread.ErrKtavMigrationRecord(), filepath.Base(path), err)
			}
			docread.WarnMigrationDeferredOnce(path, err.Error())
		}
	}
	return nil
}

func ktavMigrateWorkspaceIfNeeded(ctx context.Context, workingDir, dataDir string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// KTAV-4b: the data directory also holds the provider/model-catalog
	// caches. Their conversion and pending-transaction recovery is
	// INDEPENDENT of the workspace config's format: it must run whenever
	// the gate is on, before any early return below, under the SAME shared
	// startup deadline.
	ktavMigrateCacheArtifactsStartup(ctx, workingDir)
	legacy := filepath.Join(dataDir, appName+".json")
	if docread.KtavEntryExists(docread.MigrationRecordPathFor(legacy)) {
		if err := runKtavMigrationState(ctx, legacy, workingDir); err != nil {
			return err
		}
		return nil
	}
	if !ktavAutoMigrate || docread.FileExists(docread.KtavSiblingOf(legacy)) || !docread.FileExists(legacy) {
		return nil
	}
	if err := runKtavMigrationState(ctx, legacy, workingDir); err != nil {
		state := inspectKtavMigrationEntries(legacy)
		if !state.recordExists && !state.ktavExists && !state.backupExists && !errors.Is(err, docread.ErrKtavMigrationConflict()) && !errors.Is(err, docread.ErrKtavBackupOnly()) && !errors.Is(err, docread.ErrKtavMigrationRecord()) {
			docread.WarnMigrationDeferredOnce(legacy, err.Error())
			return nil
		}
		return err
	}
	return nil
}

// SetKtavAutoMigrateForTest is a test seam ONLY: it sets the slotmigrate-local
// ktavAutoMigrate mirror and returns a func restoring the previous value. The
// mirror is kept in sync by slots.SetKtavAutoMigrateForTest; production code
// never writes it and reads the default true.
func SetKtavAutoMigrateForTest(on bool) (restore func()) {
	prev := ktavAutoMigrate
	ktavAutoMigrate = on
	return func() { ktavAutoMigrate = prev }
}

// KtavMigrateStartupSlots converts every pending config slot candidate at
// startup. Exported wrapper over ktavMigrateStartupSlots.
func KtavMigrateStartupSlots(ctx context.Context, workingDir string) error {
	return ktavMigrateStartupSlots(ctx, workingDir)
}

// KtavMigrateWorkspaceIfNeeded migrates the workspace config and cache
// artifacts. Exported wrapper over ktavMigrateWorkspaceIfNeeded.
func KtavMigrateWorkspaceIfNeeded(ctx context.Context, workingDir, dataDir string) error {
	return ktavMigrateWorkspaceIfNeeded(ctx, workingDir, dataDir)
}

// RunKtavMigrationState runs the crash-recoverable migration state machine
// for one legacy file. Exported wrapper over runKtavMigrationState.
func RunKtavMigrationState(ctx context.Context, legacy, workingDir string) error {
	return runKtavMigrationState(ctx, legacy, workingDir)
}

// LoadKtavMigrationRecord loads and validates the migration record for a
// legacy path. Exported wrapper over loadKtavMigrationRecord.
func LoadKtavMigrationRecord(recordPath, legacy, dest, workingDir string) (*KtavMigrationRecord, error) {
	return loadKtavMigrationRecord(recordPath, legacy, dest, workingDir)
}

// ArchiveKtavBackup archives the legacy JSON under its retained backup name.
// Exported wrapper over the platform-specific archiveKtavBackup.
func ArchiveKtavBackup(ctx context.Context, sourcePath, backupPath string, expected docread.Fingerprint, owner int, enforceOwner bool) error {
	return archiveKtavBackup(ctx, sourcePath, backupPath, expected, owner, enforceOwner)
}

// ErrKtavMigrationCrashInjected returns the crash-injected test sentinel.
// Exported accessor over errKtavMigrationCrashInjected.
func ErrKtavMigrationCrashInjected() error { return errKtavMigrationCrashInjected }

// SetKtavMigrationStepHookForTest installs the migration step test hook and
// returns the previous hook plus a restore func.
func SetKtavMigrationStepHookForTest(hook func(step string) error) (prev func(string) error, restore func()) {
	prev = ktavMigrationStepHook
	ktavMigrationStepHook = hook
	return prev, func() { ktavMigrationStepHook = prev }
}

// GetKtavMigrationStepHookForTest returns the currently installed step hook.
func GetKtavMigrationStepHookForTest() func(string) error { return ktavMigrationStepHook }

// SetKtavMigrationBudgetForTest overrides the shared migration budget and
// returns the previous value plus a restore func.
func SetKtavMigrationBudgetForTest(d time.Duration) (prev time.Duration, restore func()) {
	prev = ktavMigrationBudget
	ktavMigrationBudget = d
	return prev, func() { ktavMigrationBudget = prev }
}

// GetKtavMigrationBudgetForTest returns the current migration budget.
func GetKtavMigrationBudgetForTest() time.Duration { return ktavMigrationBudget }
