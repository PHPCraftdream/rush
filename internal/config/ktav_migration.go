// Ktav migration service: converts an active legacy JSON config into a
// Ktav file under a single lock-protected, crash-recoverable transaction.
package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
)

var (
	// Automatic migration remains off until KTAV-4 enables compatible writers.
	ktavAutoMigrate     = false
	ktavMigrationBudget = 15 * time.Second
)

const ktavMigrationRecordVersion = 1

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

type ktavMigrationRecord struct {
	Version      int    `json:"version"`
	Source       string `json:"source"`
	Destination  string `json:"destination"`
	Backup       string `json:"backup"`
	SourceDigest string `json:"source_digest"`
	StagedDigest string `json:"staged_digest"`
	SourceID     string `json:"source_id"`
	ParentID     string `json:"parent_id"`
}

type ktavMigrationEntries struct {
	jsonExists   bool
	ktavExists   bool
	backupExists bool
	recordExists bool
}

func inspectKtavMigrationEntries(legacy string) ktavMigrationEntries {
	return ktavMigrationEntries{
		jsonExists:   ktavEntryExists(legacy),
		ktavExists:   ktavEntryExists(ktavSiblingOf(legacy)),
		backupExists: ktavEntryExists(backupPathFor(legacy)),
		recordExists: ktavEntryExists(migrationRecordPathFor(legacy)),
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

func runKtavMigrationStateLocked(ctx context.Context, legacy, workingDir string) error {
	if err := runKtavMigrationStep("before-state-inspection"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("ktav migration: budget exhausted: %w", err)
	}
	backup := backupPathFor(legacy)
	recordPath := migrationRecordPathFor(legacy)
	state := inspectKtavMigrationEntries(legacy)
	ktavPath := ktavSiblingOf(legacy)
	switch {
	case !state.ktavExists && !state.jsonExists && !state.recordExists && !state.backupExists:
		return nil
	case !state.ktavExists && !state.jsonExists && state.backupExists && !state.recordExists:
		return fmt.Errorf("%w: %s exists without %s or %s", errKtavBackupOnly, filepath.Base(backup), filepath.Base(legacy), filepath.Base(ktavPath))
	case state.ktavExists && state.backupExists && !state.jsonExists:
		if !state.recordExists {
			return nil
		}
		record, err := loadKtavMigrationRecord(recordPath, legacy, workingDir)
		if err != nil {
			return err
		}
		owner, enforce, err := ownerPolicyForMigration(legacy, workingDir)
		if err != nil {
			return err
		}
		if err := verifyKtavGenerations(record, legacy, owner, enforce, state); err != nil {
			return err
		}
		if err := syncConfigParent(filepath.Dir(recordPath)); err != nil {
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
		if _, err := loadKtavMigrationRecord(recordPath, legacy, workingDir); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s and its migration record exist but source and backup are gone", errKtavMigrationConflict, filepath.Base(ktavPath))
	case state.jsonExists && state.backupExists:
		return fmt.Errorf("%w: unexpected backup %s beside active %s; preserve every file and resolve manually", errKtavMigrationConflict, filepath.Base(backup), filepath.Base(legacy))
	case state.ktavExists && state.jsonExists:
		if !state.recordExists {
			return fmt.Errorf("%w: Ktav and JSON coexist without an authenticated migration record", errKtavMigrationConflict)
		}
		record, err := loadKtavMigrationRecord(recordPath, legacy, workingDir)
		if err != nil {
			return err
		}
		owner, enforce, err := ownerPolicyForMigration(legacy, workingDir)
		if err != nil {
			return err
		}
		if err := verifyKtavGenerations(record, legacy, owner, enforce, state); err != nil {
			return err
		}
		return finishKtavArchival(ctx, legacy, backup, recordPath, record, workingDir)
	}
	return runKtavMigrationTransaction(ctx, legacy, backup, recordPath, workingDir)
}

func runKtavMigrationTransaction(ctx context.Context, legacy, backup, recordPath, workingDir string) error {
	state := inspectKtavMigrationEntries(legacy)
	var priorRecord *ktavMigrationRecord
	if state.recordExists {
		var err error
		priorRecord, err = loadKtavMigrationRecord(recordPath, legacy, workingDir)
		if err != nil {
			return err
		}
	}
	if state.backupExists {
		return fmt.Errorf("%w: backup %s already exists", errKtavMigrationConflict, filepath.Base(backup))
	}
	owner, enforce, err := ownerPolicyForMigration(legacy, workingDir)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source, sourceFP, err := readStableConfigFileOwned(legacy, owner, enforce)
	if err != nil {
		return fmt.Errorf("ktav migration: failed to read %s: %w", legacy, err)
	}
	if sourceFP.nlink != 1 {
		return ErrConfigHardLink
	}
	if priorRecord != nil && (!validKtavMigrationDigest(priorRecord.SourceDigest) || fmt.Sprintf("%v", sourceFP.identity) != priorRecord.SourceID || fmt.Sprintf("%v", sourceFP.parentIdentity) != priorRecord.ParentID || hex.EncodeToString(sourceFP.digest[:]) != priorRecord.SourceDigest) {
		return fmt.Errorf("%w: existing record does not match source %s", errKtavMigrationConflict, filepath.Base(legacy))
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
	if err := ktavMigrationCheckContext(ctx, "after-convert"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stagedDigest := sha256.Sum256(ktavBytes)
	record := ktavMigrationRecord{
		Version:      ktavMigrationRecordVersion,
		Source:       filepath.Base(legacy),
		Destination:  filepath.Base(ktavSiblingOf(legacy)),
		Backup:       filepath.Base(backup),
		SourceDigest: hex.EncodeToString(sourceFP.digest[:]),
		StagedDigest: hex.EncodeToString(stagedDigest[:]),
		SourceID:     fmt.Sprintf("%v", sourceFP.identity),
		ParentID:     fmt.Sprintf("%v", sourceFP.parentIdentity),
	}
	if priorRecord != nil {
		if fmt.Sprintf("%v", sourceFP.identity) != priorRecord.SourceID || fmt.Sprintf("%v", sourceFP.parentIdentity) != priorRecord.ParentID || hex.EncodeToString(sourceFP.digest[:]) != priorRecord.SourceDigest || hex.EncodeToString(stagedDigest[:]) != priorRecord.StagedDigest {
			return fmt.Errorf("%w: existing record does not match regenerated transaction", errKtavMigrationConflict)
		}
		record = *priorRecord
	}
	recordBytes, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("ktav migration: failed to encode record: %w", err)
	}
	ktavPath := ktavSiblingOf(legacy)
	_, expected, err := readStableConfigFileOwned(ktavPath, owner, enforce)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("ktav migration: failed to inspect %s: %w", ktavPath, err)
	}
	if expected.exists {
		return fmt.Errorf("%w: %s appeared during migration", errKtavMigrationConflict, filepath.Base(ktavPath))
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
			absent := reloadFileFingerprint{
				discovery:       configDiscoveryFingerprint(recordPath),
				parentDiscovery: configDiscoveryFingerprint(filepath.Dir(recordPath)),
				parentIdentity:  configParentIdentity(recordPath),
				aliasChain:      configAliasChainFingerprint(recordPath),
			}
			if !absent.parentIdentity.valid {
				return fmt.Errorf("ktav migration: cannot pin record parent %s", filepath.Dir(recordPath))
			}
			if err := ktavMigrationCheckContext(ctx, "record-commit"); err != nil {
				return err
			}
			if _, err := commitConfigFile(recordPath, recordPath, append(recordBytes, '\n'), 0o600, absent, owner, enforce); err != nil {
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
	if _, err := commitConfigFile(ktavPath, ktavPath, ktavBytes, 0o600, expected, owner, enforce, publishHook); err != nil {
		return fmt.Errorf("ktav migration: failed to publish %s: %w", ktavPath, err)
	}
	if err := ktavMigrationCheckContext(ctx, "after-publish"); err != nil {
		return err
	}
	return finishKtavArchival(ctx, legacy, backup, recordPath, &record, workingDir)
}

func finishKtavArchival(ctx context.Context, legacy, backup, recordPath string, record *ktavMigrationRecord, workingDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, enforce, err := ownerPolicyForMigration(legacy, workingDir)
	if err != nil {
		return err
	}
	if !validKtavMigrationDigest(record.SourceDigest) {
		return fmt.Errorf("%w: migration record source digest is malformed", errKtavMigrationRecord)
	}
	if err := verifyKtavGenerations(record, legacy, owner, enforce, inspectKtavMigrationEntries(legacy)); err != nil {
		return err
	}
	current, currentFP, err := readStableConfigFileOwned(legacy, owner, enforce)
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
	if _, err := hex.Decode(want[:], []byte(record.SourceDigest)); err != nil || currentFP.digest != want || !sameBytesFingerprint(current, want) || fmt.Sprintf("%v", currentFP.identity) != record.SourceID || fmt.Sprintf("%v", currentFP.parentIdentity) != record.ParentID {
		return fmt.Errorf("%w: %s changed after Ktav publication; the JSON stays active", errKtavMigrationConflict, filepath.Base(legacy))
	}
	if err := archiveKtavBackup(ctx, legacy, backup, currentFP, owner, enforce); err != nil {
		if errors.Is(err, errKtavMigrationConflict) {
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

func loadKtavMigrationRecord(recordPath, legacy, workingDir string) (*ktavMigrationRecord, error) {
	owner, enforce, err := ownerPolicyForMigration(legacy, workingDir)
	if err != nil {
		return nil, err
	}
	recordBytes, recordFP, err := readStableConfigFileOwned(recordPath, owner, enforce)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: record %s vanished during recovery", errKtavMigrationRecord, filepath.Base(recordPath))
		}
		return nil, fmt.Errorf("%w: cannot read migration record %s: %v", errKtavMigrationRecord, filepath.Base(recordPath), err)
	}
	var record ktavMigrationRecord
	if err := json.Unmarshal(recordBytes, &record); err != nil {
		return nil, fmt.Errorf("%w: unreadable migration record %s", errKtavMigrationRecord, filepath.Base(recordPath))
	}
	if record.Version != ktavMigrationRecordVersion || record.Source != filepath.Base(legacy) || record.Destination != filepath.Base(ktavSiblingOf(legacy)) || record.Backup != filepath.Base(backupPathFor(legacy)) || record.SourceID == "" || record.ParentID == "" || !validKtavMigrationDigest(record.SourceDigest) || !validKtavMigrationDigest(record.StagedDigest) || recordFP.parentIdentity != configParentIdentity(recordPath) || !recordFP.identity.valid || recordFP.parentIdentity != configParentIdentity(legacy) {
		return nil, fmt.Errorf("%w: migration record %s is malformed or bound to a different slot", errKtavMigrationRecord, filepath.Base(recordPath))
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

func verifyKtavGenerations(record *ktavMigrationRecord, legacy string, owner int, enforceOwner bool, state ktavMigrationEntries) error {
	if !validKtavMigrationDigest(record.StagedDigest) || !validKtavMigrationDigest(record.SourceDigest) {
		return fmt.Errorf("%w: migration record digest is malformed", errKtavMigrationRecord)
	}
	var staged [sha256.Size]byte
	if _, err := hex.Decode(staged[:], []byte(record.StagedDigest)); err != nil {
		return fmt.Errorf("%w: staged digest is malformed", errKtavMigrationRecord)
	}
	ktavData, ktavFP, err := readStableConfigFileOwned(ktavSiblingOf(legacy), owner, enforceOwner)
	if err != nil {
		return fmt.Errorf("%w: cannot verify published Ktav %s: %v", errKtavMigrationRecord, filepath.Base(ktavSiblingOf(legacy)), err)
	}
	if ktavFP.digest != staged || !sameBytesFingerprint(ktavData, staged) {
		return fmt.Errorf("%w: migration record does not match published %s", errKtavMigrationRecord, filepath.Base(ktavSiblingOf(legacy)))
	}
	if state.jsonExists {
		data, fp, err := readStableConfigFileOwned(legacy, owner, enforceOwner)
		if err != nil {
			return fmt.Errorf("%w: cannot verify source %s: %v", errKtavMigrationRecord, filepath.Base(legacy), err)
		}
		var want [sha256.Size]byte
		if _, err := hex.Decode(want[:], []byte(record.SourceDigest)); err != nil || fp.digest != want || !sameBytesFingerprint(data, want) || fmt.Sprintf("%v", fp.identity) != record.SourceID || fmt.Sprintf("%v", fp.parentIdentity) != record.ParentID {
			return fmt.Errorf("%w: migration record does not match source %s", errKtavMigrationConflict, filepath.Base(legacy))
		}
	}
	if state.backupExists {
		data, fp, err := readStableConfigFileOwned(backupPathFor(legacy), owner, enforceOwner)
		if err != nil {
			return fmt.Errorf("%w: cannot verify backup %s: %v", errKtavMigrationRecord, filepath.Base(backupPathFor(legacy)), err)
		}
		var want [sha256.Size]byte
		if _, err := hex.Decode(want[:], []byte(record.SourceDigest)); err != nil || fp.digest != want || !sameBytesFingerprint(data, want) || fmt.Sprintf("%v", fp.identity) != record.SourceID || fmt.Sprintf("%v", fp.parentIdentity) != record.ParentID {
			return fmt.Errorf("%w: migration record does not match backup %s", errKtavMigrationRecord, filepath.Base(backupPathFor(legacy)))
		}
	}
	return nil
}

func removeKtavMigrationRecord(recordPath string) error {
	if err := os.Remove(recordPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("ktav migration: failed to remove record %s: %w", recordPath, err)
	}
	return syncConfigParent(filepath.Dir(recordPath))
}

func ownerPolicyForMigration(path, workingDir string) (int, bool, error) {
	canonical := normalizeReloadPath(path)
	if systemConfigPath != "" && matchesLogicalSlot(canonical, systemConfigPath) || matchesLogicalSlot(canonical, GlobalConfig()) || matchesLogicalSlot(canonical, GlobalConfigData()) {
		return configOwnerPolicyForPath(path, workingDir)
	}
	projectDir := workingDir
	if projectDir == "" {
		projectDir = filepath.Dir(path)
	}
	return configOwnerPolicyForPath(path, projectDir)
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
	for _, legacy := range ktavMigrationPendingSlots(workingDir) {
		if err := runKtavMigrationState(ctx, legacy, workingDir); err != nil {
			return fmt.Errorf("%w: cannot recover pending transaction for %s: %v", errKtavMigrationRecord, filepath.Base(legacy), err)
		}
	}
	if !ktavAutoMigrate {
		return nil
	}
	for _, path := range lookupConfigCandidates(workingDir) {
		if path == "" || isKtavConfigPath(path) || isKtavBackupPath(path) || isKtavMigrationRecordPath(path) {
			continue
		}
		if fileExists(ktavSiblingOf(path)) {
			continue
		}
		if !fileExists(path) {
			continue
		}
		if err := runKtavMigrationState(ctx, path, workingDir); err != nil {
			if errors.Is(err, errKtavMigrationConflict) || errors.Is(err, errKtavBackupOnly) || errors.Is(err, errKtavMigrationRecord) {
				return err
			}
			if ktavEntryExists(migrationRecordPathFor(path)) || ktavEntryExists(ktavSiblingOf(path)) {
				return fmt.Errorf("%w: cannot continue migration for %s: %v", errKtavMigrationRecord, filepath.Base(path), err)
			}
			warnMigrationDeferredOnce(path, err.Error())
		}
	}
	return nil
}

func ktavMigrateWorkspaceIfNeeded(ctx context.Context, workingDir, dataDir string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	legacy := filepath.Join(dataDir, appName+".json")
	if ktavEntryExists(migrationRecordPathFor(legacy)) {
		return runKtavMigrationState(ctx, legacy, workingDir)
	}
	if !ktavAutoMigrate || fileExists(ktavSiblingOf(legacy)) || !fileExists(legacy) {
		return nil
	}
	if err := runKtavMigrationState(ctx, legacy, workingDir); err != nil {
		state := inspectKtavMigrationEntries(legacy)
		if !state.recordExists && !state.ktavExists && !state.backupExists && !errors.Is(err, errKtavMigrationConflict) && !errors.Is(err, errKtavBackupOnly) && !errors.Is(err, errKtavMigrationRecord) {
			warnMigrationDeferredOnce(legacy, err.Error())
			return nil
		}
		return err
	}
	return nil
}
