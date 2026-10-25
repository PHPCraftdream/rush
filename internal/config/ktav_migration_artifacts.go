// Ktav sidecar-artifact migration: OAuth recovery journals and
// provider/model-catalog caches are converted JSON -> Ktav through the SAME
// transaction primitives as the config migration (commitConfigFile,
// archiveKtavBackup, the shared ktavMigrationRecord format, loadKtavMigrationRecord
// and finishKtavArchival). Artifacts are not authority data, so recovery may be
// coarser than the config table — but it must never lose bytes: after any
// interruption either the JSON is intact and active, or the Ktav file and its
// backup both exist. A pre-existing backup or Ktav file is never replaced; a
// conflict leaves every file in place with the JSON still active.
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

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
)

// ktavArtifactMigrationStepHook is the crash-boundary test hook for the
// artifact transaction (named like ktavMigrationStepHook). It never fires
// for the config-document migration.
var ktavArtifactMigrationStepHook func(step string) error

func runKtavArtifactMigrationStep(step string) error {
	if ktavArtifactMigrationStepHook != nil {
		return ktavArtifactMigrationStepHook(step)
	}
	return nil
}

func ktavArtifactCheckContext(ctx context.Context, step string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("ktav artifact migration %s: %w", step, err)
	}
	return runKtavArtifactMigrationStep(step)
}

// migrateKtavArtifact converts one sidecar artifact named by its legacy JSON
// path under the artifact's own legacy sidecar lock (legacy + ".lock").
// Callers pass the shared startup deadline context; a budget is applied only
// when the context carries none, so startup never adds per-file timeouts.
func migrateKtavArtifact(ctx context.Context, legacy, workingDir string) error {
	return migrateKtavArtifactWithLock(ctx, legacy, workingDir, legacy+".lock")
}

// migrateKtavArtifactWithLock converts one sidecar artifact under an explicit
// sidecar lock path — OAuth journals pass the same legacy-anchored refresh
// sidecar lock the recovery worker uses.
func migrateKtavArtifactWithLock(ctx context.Context, legacy, workingDir, lockPath string) error {
	if legacy == "" {
		return fmt.Errorf("ktav artifact migration: empty legacy path")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ktavMigrationBudget)
		defer cancel()
	}
	lock, err := acquireConfigFileLock(ctx, lockPath)
	if err != nil {
		return fmt.Errorf("ktav artifact migration: failed to lock %s: %w", filepath.Base(lockPath), err)
	}
	defer func() { _ = releaseConfigFileLock(lockPath, lock) }()
	if err := runKtavArtifactMigrationStep("after-lock"); err != nil {
		return err
	}
	return runKtavArtifactMigrationStateLocked(ctx, legacy, workingDir)
}

func runKtavArtifactMigrationStateLocked(ctx context.Context, legacy, workingDir string) error {
	if err := runKtavArtifactMigrationStep("before-state-inspection"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("ktav artifact migration: budget exhausted: %w", err)
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
		// Both post-migration generations survive: no bytes can be lost, so
		// recovery only has to retire an authenticated leftover record.
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
		if err := runKtavArtifactMigrationStep("after-archive"); err != nil {
			return err
		}
		ktavArtifactPreserveMTime(ktavPath, backup)
		return removeKtavMigrationRecord(recordPath)
	case state.ktavExists && !state.jsonExists && !state.backupExists:
		if !state.recordExists {
			// A Ktav sibling written by the format-aware cache/journal
			// writers without any migration transaction.
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
			return fmt.Errorf("%w: Ktav and JSON coexist without an authenticated migration record; %s stays active", errKtavMigrationConflict, filepath.Base(legacy))
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
		return ktavArtifactFinishArchival(ctx, legacy, backup, recordPath, record, workingDir)
	}
	return runKtavArtifactMigrationTransaction(ctx, legacy, backup, recordPath, workingDir)
}

func runKtavArtifactMigrationTransaction(ctx context.Context, legacy, backup, recordPath, workingDir string) error {
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
		return fmt.Errorf("ktav artifact migration: failed to read %s: %w", filepath.Base(legacy), err)
	}
	if sourceFP.nlink != 1 {
		return ErrConfigHardLink
	}
	if priorRecord != nil && (!validKtavMigrationDigest(priorRecord.SourceDigest) || fmt.Sprintf("%v", sourceFP.identity) != priorRecord.SourceID || fmt.Sprintf("%v", sourceFP.parentIdentity) != priorRecord.ParentID || hex.EncodeToString(sourceFP.digest[:]) != priorRecord.SourceDigest) {
		return fmt.Errorf("%w: existing record does not match source %s", errKtavMigrationConflict, filepath.Base(legacy))
	}
	if err := ktavArtifactCheckContext(ctx, "after-read"); err != nil {
		return err
	}
	if err := ktavArtifactCheckContext(ctx, "before-convert"); err != nil {
		return err
	}
	// ConvertJSONToKtav parses losslessly, encodes, re-decodes strictly, and
	// proves semantic equality before returning any bytes.
	ktavBytes, err := ktavcodec.ConvertJSONToKtav(source)
	if err != nil {
		return fmt.Errorf("ktav conversion failed for %s: %w", filepath.Base(legacy), err)
	}
	if err := ktavArtifactCheckContext(ctx, "after-convert"); err != nil {
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
			return fmt.Errorf("%w: existing record does not match regenerated transaction", errKtavMigrationConflict, filepath.Base(legacy))
		}
		record = *priorRecord
	}
	recordBytes, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("ktav artifact migration: failed to encode record: %w", err)
	}
	ktavPath := ktavSiblingOf(legacy)
	_, expected, err := readStableConfigFileOwned(ktavPath, owner, enforce)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("ktav artifact migration: failed to inspect %s: %w", filepath.Base(ktavPath), err)
	}
	if expected.exists {
		return fmt.Errorf("%w: %s appeared during migration", errKtavMigrationConflict, filepath.Base(ktavPath))
	}
	publishHook := func(step string) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("ktav artifact migration %s: %w", step, err)
		}
		switch step {
		case "before-staging":
			return runKtavArtifactMigrationStep("before-staging")
		case "during-staging":
			return runKtavArtifactMigrationStep("during-staging")
		case "after-staging":
			if priorRecord != nil {
				return nil
			}
			if err := ktavArtifactCheckContext(ctx, "before-record"); err != nil {
				return err
			}
			absent := reloadFileFingerprint{
				discovery:       configDiscoveryFingerprint(recordPath),
				parentDiscovery: configDiscoveryFingerprint(filepath.Dir(recordPath)),
				parentIdentity:  configParentIdentity(recordPath),
				aliasChain:      configAliasChainFingerprint(recordPath),
			}
			if !absent.parentIdentity.valid {
				return fmt.Errorf("ktav artifact migration: cannot pin record parent %s", filepath.Dir(recordPath))
			}
			if err := ktavArtifactCheckContext(ctx, "record-commit"); err != nil {
				return err
			}
			if _, err := commitConfigFile(recordPath, recordPath, append(recordBytes, '\n'), 0o600, absent, owner, enforce); err != nil {
				return fmt.Errorf("ktav artifact migration: failed to persist record %s: %w", filepath.Base(recordPath), err)
			}
			return ktavArtifactCheckContext(ctx, "after-record")
		case "after-rename":
			return ktavArtifactCheckContext(ctx, "after-ktav-rename")
		}
		return nil
	}
	if err := ktavArtifactCheckContext(ctx, "before-ktav-rename"); err != nil {
		return err
	}
	if _, err := commitConfigFile(ktavPath, ktavPath, ktavBytes, 0o600, expected, owner, enforce, publishHook); err != nil {
		return fmt.Errorf("ktav artifact migration: failed to publish %s: %w", filepath.Base(ktavPath), err)
	}
	// Provider/hyper cache freshness is mtime-based (cache.Age), so the new
	// Ktav inherits the source's mtime. Best-effort: losing it can only make
	// a cache look fresher, never lose bytes.
	ktavArtifactPreserveMTime(ktavPath, legacy)
	if err := ktavArtifactCheckContext(ctx, "after-publish"); err != nil {
		return err
	}
	return ktavArtifactFinishArchival(ctx, legacy, backup, recordPath, &record, workingDir)
}

// ktavArtifactFinishArchival archives the JSON to its retained backup through
// the shared, identity-checked finishKtavArchival and retires the record.
func ktavArtifactFinishArchival(ctx context.Context, legacy, backup, recordPath string, record *ktavMigrationRecord, workingDir string) error {
	if err := ktavArtifactCheckContext(ctx, "before-archive"); err != nil {
		return err
	}
	if err := finishKtavArchival(ctx, legacy, backup, recordPath, record, workingDir); err != nil {
		return err
	}
	if err := runKtavArtifactMigrationStep("after-archive"); err != nil {
		return err
	}
	// Re-apply the source mtime in case a crash between publish and the
	// first Chtimes left the Ktav with its publication time.
	ktavArtifactPreserveMTime(ktavSiblingOf(legacy), backup)
	return nil
}

// ktavArtifactPreserveMTime copies the source file's mtime onto the target.
// Best-effort by design: a failure only affects freshness heuristics.
func ktavArtifactPreserveMTime(ktavPath, sourcePath string) {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return
	}
	mod := info.ModTime()
	_ = os.Chtimes(ktavPath, mod, mod)
}

// ktavMigrateOAuthJournalsAtStartup converts every rotating provider's legacy
// JSON journal for the given config slots BEFORE any recovery worker starts.
// It is gated on ktavAutoMigrate and never fails a load: any error defers the
// conversion with a once-per-path, paths-only warning while the JSON journal
// stays active.
func ktavMigrateOAuthJournalsAtStartup(ctx context.Context, configSlots []string, workingDir string) {
	if !ktavAutoMigrate {
		return
	}
	for _, slot := range configSlots {
		canonical := legacySlotOf(normalizeReloadPath(slot))
		if canonical == "" {
			continue
		}
		for _, descriptor := range oauthProviderDescriptors() {
			if !descriptor.rotating {
				continue
			}
			legacy := oauthJournalLegacyPath(canonical, descriptor.id)
			if !ktavEntryExists(legacy) {
				continue
			}
			if err := ktavMigrateOAuthJournal(ctx, canonical, descriptor.id, workingDir); err != nil {
				warnMigrationDeferredOnce(legacy, err.Error())
			}
		}
	}
}

// ktavMigrateOAuthJournal converts one provider's pending journal under the
// SAME legacy-anchored refresh sidecar lock the recovery worker uses.
func ktavMigrateOAuthJournal(ctx context.Context, configPath, providerID, workingDir string) error {
	legacy := oauthJournalLegacyPath(configPath, providerID)
	// A journal that is absent, unreadable, or invalid stays exactly where it
	// is: invalid journals belong to the recovery worker's quarantine path.
	record, err := readOAuthJournalWithRetry(ctx, legacy)
	if err != nil {
		if errors.Is(err, errOAuthJournalInvalid) {
			return nil
		}
		return err
	}
	if record == nil {
		return nil
	}
	if err := migrateKtavArtifactWithLock(ctx, legacy, workingDir, oauthRefreshLockPath(configPath, providerID)); err != nil {
		return err
	}
	// Prove the converted journal still decodes to the identical JSON-shaped
	// record: the retained backup and the published Ktav must be Equivalent.
	backupBytes, err := os.ReadFile(backupPathFor(legacy))
	if err != nil {
		return fmt.Errorf("ktav journal migration: cannot read backup %s: %w", filepath.Base(backupPathFor(legacy)), err)
	}
	sourceTree, err := ktavcodec.ParseJSONLossless(backupBytes)
	if err != nil {
		return fmt.Errorf("ktav journal migration: backup %s is not the original record: %w", filepath.Base(backupPathFor(legacy)), err)
	}
	ktavRaw, err := os.ReadFile(oauthJournalKtavPath(configPath, providerID))
	if err != nil {
		return fmt.Errorf("ktav journal migration: cannot read %s: %w", filepath.Base(oauthJournalKtavPath(configPath, providerID)), err)
	}
	ktavTree, err := ktavcodec.DecodeKtav(ktavRaw)
	if err != nil {
		return fmt.Errorf("ktav journal migration: invalid Ktav in %s: %w", filepath.Base(oauthJournalKtavPath(configPath, providerID)), err)
	}
	if err := ktavcodec.Equivalent(sourceTree, ktavTree); err != nil {
		return fmt.Errorf("ktav journal migration: %s does not preserve the original record: %w", filepath.Base(oauthJournalKtavPath(configPath, providerID)), err)
	}
	return nil
}

// ktavMigrateCacheArtifactsAtStartup converts the provider/hyper catalogs and
// every model-catalog cache in the data directory. Gated on ktavAutoMigrate;
// failures defer with a paths-only warning and the JSON stays active.
func ktavMigrateCacheArtifactsAtStartup(ctx context.Context, workingDir string) {
	if !ktavAutoMigrate {
		return
	}
	for _, legacy := range ktavCacheArtifactPaths() {
		if !ktavEntryExists(legacy) {
			continue
		}
		if err := migrateKtavArtifact(ctx, legacy, workingDir); err != nil {
			warnMigrationDeferredOnce(legacy, err.Error())
		}
	}
}

// ktavCacheArtifactPaths lists the Rush-owned cache slots by their legacy
// JSON paths: providers.json, hyper.json, model-catalog-<id>.json.
func ktavCacheArtifactPaths() []string {
	providers := cachePathFor("providers")
	paths := []string{providers, cachePathFor("hyper")}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(providers), "model-catalog-*.json"))
	if err == nil {
		paths = append(paths, matches...)
	}
	return paths
}
