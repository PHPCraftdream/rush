// Ktav sidecar-artifact migration: OAuth recovery journals and
// provider/model-catalog caches are converted JSON -> Ktav through the SAME
// transaction primitives as the config migration (commitConfigFile,
// archiveKtavBackup, the shared ktavMigrationRecord format, loadKtavMigrationRecord
// and finishKtavArchival). Artifacts are not authority data, so recovery may be
// coarser than the config table — but it must never lose bytes: after any
// interruption either the JSON is intact and active, or the Ktav file and its
// backup both exist. A pre-existing backup or Ktav file is never replaced; a
// conflict leaves every file in place, and because every reader prefers the
// Ktav generation, an existing Ktav file stays the ACTIVE generation while the
// JSON copy is ignored until it is resolved manually.
package config

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

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
)

// ktavArtifactMigrationStepHook is the crash-boundary test hook for the
// artifact transaction (named like ktavMigrationStepHook). It never fires
// for the config-document migration.
var ktavArtifactMigrationStepHook func(step string) error

// Seams for the mtime-preservation step so tests can inject failures.
var (
	ktavArtifactStatFile = os.Stat
	ktavArtifactChtimes  = os.Chtimes
)

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

// ktavArtifactMigrationEntries reports the on-disk generations for one
// artifact. It is destination-aware: unlike the config table, a journal's
// published Ktav name is NOT the trailing-suffix Ktav sibling of its legacy
// JSON name (it is oauthJournalKtavPath), so the state machine must inspect
// exactly the destination the readers resolve.
func ktavArtifactMigrationEntries(legacy, dest string) ktavMigrationEntries {
	return ktavMigrationEntries{
		jsonExists:   ktavEntryExists(legacy),
		ktavExists:   ktavEntryExists(dest),
		backupExists: ktavEntryExists(backupPathFor(legacy)),
		recordExists: ktavEntryExists(migrationRecordPathFor(legacy)),
	}
}

// migrateKtavArtifact converts one sidecar artifact named by its legacy JSON
// path under the artifact's own legacy sidecar lock (legacy + ".lock").
// Callers pass the shared startup deadline context; a budget is applied only
// when the context carries none, so startup never adds per-file timeouts.
func migrateKtavArtifact(ctx context.Context, legacy, workingDir string) error {
	return migrateKtavArtifactToWithLock(ctx, legacy, ktavSiblingOf(legacy), workingDir, legacy+".lock", nil)
}

// migrateKtavArtifactWithLock converts one sidecar artifact under an explicit
// sidecar lock path, publishing to the legacy JSON's Ktav sibling.
func migrateKtavArtifactWithLock(ctx context.Context, legacy, workingDir, lockPath string) error {
	return migrateKtavArtifactToWithLock(ctx, legacy, ktavSiblingOf(legacy), workingDir, lockPath, nil)
}

// migrateKtavArtifactToWithLock converts one sidecar artifact under an
// explicit sidecar lock path and publishes the Ktav generation to dest —
// ONE destination resolved by ONE source of truth: journals pass
// oauthJournalKtavPath (the path selectOAuthJournalPath reads), caches pass
// the Ktav sibling. validateUnderLock, when non-nil, re-validates the exact
// bytes read under the lock BEFORE conversion; an error skips the migration
// (the artifact is left untouched, e.g. an invalid journal left for
// quarantine).
func migrateKtavArtifactToWithLock(ctx context.Context, legacy, dest, workingDir, lockPath string, validateUnderLock func([]byte) error) error {
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
	return runKtavArtifactMigrationStateLocked(ctx, legacy, dest, workingDir, validateUnderLock)
}

func runKtavArtifactMigrationStateLocked(ctx context.Context, legacy, dest, workingDir string, validateUnderLock func([]byte) error) error {
	if err := runKtavArtifactMigrationStep("before-state-inspection"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("ktav artifact migration: budget exhausted: %w", err)
	}
	backup := backupPathFor(legacy)
	recordPath := migrationRecordPathFor(legacy)
	state := ktavArtifactMigrationEntries(legacy, dest)
	switch {
	case !state.ktavExists && !state.jsonExists && !state.recordExists && !state.backupExists:
		return nil
	case !state.ktavExists && !state.jsonExists && state.backupExists && !state.recordExists:
		return fmt.Errorf("%w: %s exists without %s or %s", errKtavBackupOnly, filepath.Base(backup), filepath.Base(legacy), filepath.Base(dest))
	case state.ktavExists && state.backupExists && !state.jsonExists:
		// Both post-migration generations survive: no bytes can be lost, so
		// recovery only has to retire an authenticated leftover record.
		if !state.recordExists {
			return nil
		}
		record, err := loadKtavMigrationRecord(recordPath, legacy, dest, workingDir)
		if err != nil {
			return err
		}
		owner, enforce, err := ownerPolicyForMigration(legacy, workingDir)
		if err != nil {
			return err
		}
		if err := verifyKtavGenerations(record, legacy, dest, owner, enforce, state); err != nil {
			return err
		}
		if err := syncConfigParent(filepath.Dir(recordPath)); err != nil {
			return err
		}
		if err := runKtavArtifactMigrationStep("after-archive"); err != nil {
			return err
		}
		// Tolerant re-apply: the JSON is already gone, so a failure here can
		// only affect freshness heuristics — it must not fail the recovery.
		if err := ktavArtifactPreserveMTime(dest, backup); err != nil {
			warnMigrationDeferredOnce(dest, err.Error())
		}
		return removeKtavMigrationRecord(recordPath)
	case state.ktavExists && !state.jsonExists && !state.backupExists:
		if !state.recordExists {
			// A Ktav file written by the format-aware cache/journal writers
			// without any migration transaction.
			return nil
		}
		if _, err := loadKtavMigrationRecord(recordPath, legacy, dest, workingDir); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s and its migration record exist but source and backup are gone", errKtavMigrationConflict, filepath.Base(dest))
	case state.jsonExists && state.backupExists:
		return fmt.Errorf("%w: unexpected backup %s beside active %s; preserve every file and resolve manually", errKtavMigrationConflict, filepath.Base(backup), filepath.Base(legacy))
	case state.ktavExists && state.jsonExists:
		if !state.recordExists {
			// Readers prefer the Ktav generation, so the EXISTING Ktav file
			// stays active; the JSON copy is ignored. Both files are left
			// untouched for manual resolution.
			return fmt.Errorf("%w: Ktav and JSON coexist without an authenticated migration record; the existing %s stays the active generation and %s is ignored", errKtavMigrationConflict, filepath.Base(dest), filepath.Base(legacy))
		}
		record, err := loadKtavMigrationRecord(recordPath, legacy, dest, workingDir)
		if err != nil {
			return err
		}
		owner, enforce, err := ownerPolicyForMigration(legacy, workingDir)
		if err != nil {
			return err
		}
		if err := verifyKtavGenerations(record, legacy, dest, owner, enforce, state); err != nil {
			return err
		}
		// Hard mtime step BEFORE archival: the published Ktav is
		// reader-preferred but the JSON still exists, so a failed timestamp
		// stop must leave the pending state (Ktav + JSON + record) untouched;
		// archival never proceeds past a persistent timestamp failure.
		if err := ktavArtifactPreserveMTime(dest, legacy); err != nil {
			return err
		}
		return ktavArtifactFinishArchival(ctx, legacy, dest, backup, recordPath, record, workingDir)
	}
	return runKtavArtifactMigrationTransaction(ctx, legacy, dest, backup, recordPath, workingDir, validateUnderLock)
}

func runKtavArtifactMigrationTransaction(ctx context.Context, legacy, dest, backup, recordPath, workingDir string, validateUnderLock func([]byte) error) error {
	state := ktavArtifactMigrationEntries(legacy, dest)
	var priorRecord *ktavMigrationRecord
	if state.recordExists {
		var err error
		priorRecord, err = loadKtavMigrationRecord(recordPath, legacy, dest, workingDir)
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
	// Re-validate the EXACT bytes this transaction would convert, now that
	// the lock is held: an artifact that became invalid while the lock was
	// contended is skipped, never converted or archived.
	if validateUnderLock != nil {
		if err := validateUnderLock(source); err != nil {
			return nil
		}
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
		Destination:  filepath.Base(dest),
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
	_, expected, err := readStableConfigFileOwned(dest, owner, enforce)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("ktav artifact migration: failed to inspect %s: %w", filepath.Base(dest), err)
	}
	if expected.exists {
		return fmt.Errorf("%w: %s appeared during migration", errKtavMigrationConflict, filepath.Base(dest))
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
	if _, err := commitConfigFile(dest, dest, ktavBytes, 0o600, expected, owner, enforce, publishHook); err != nil {
		return fmt.Errorf("ktav artifact migration: failed to publish %s: %w", filepath.Base(dest), err)
	}
	// Ktav published, archival pending; the published Ktav is
	// reader-preferred, the JSON is ignored until resolution. Provider/hyper
	// cache freshness is mtime-based (cache.Age), so the published Ktav MUST
	// inherit the source mtime. This is a hard step BEFORE the archival: on
	// failure the transaction stops with the pending state (published Ktav,
	// JSON, record) untouched, and the recovery rules converge it on the
	// next run.
	if err := ktavArtifactPreserveMTime(dest, legacy); err != nil {
		return err
	}
	if err := ktavArtifactCheckContext(ctx, "after-publish"); err != nil {
		return err
	}
	return ktavArtifactFinishArchival(ctx, legacy, dest, backup, recordPath, &record, workingDir)
}

// ktavArtifactFinishArchival archives the JSON to its retained backup through
// the shared, identity-checked finishKtavArchival and retires the record.
func ktavArtifactFinishArchival(ctx context.Context, legacy, dest, backup, recordPath string, record *ktavMigrationRecord, workingDir string) error {
	if err := ktavArtifactCheckContext(ctx, "before-archive"); err != nil {
		return err
	}
	if err := finishKtavArchival(ctx, legacy, dest, backup, recordPath, record, workingDir); err != nil {
		return err
	}
	if err := runKtavArtifactMigrationStep("after-archive"); err != nil {
		return err
	}
	// Tolerant re-apply in case a crash between publish and the first
	// Chtimes left the Ktav with its publication time: the JSON is already
	// archived, so a failure here must not report the migration as failed —
	// warn paths-only instead.
	if err := ktavArtifactPreserveMTime(dest, backup); err != nil {
		warnMigrationDeferredOnce(dest, err.Error())
	}
	return nil
}

// ktavArtifactPreserveMTime copies the source file's mtime onto the target.
// A failure to stat the source or to stamp the target is returned: for the
// cache generations the mtime IS the freshness signal, so it is a hard step,
// not best-effort.
func ktavArtifactPreserveMTime(ktavPath, sourcePath string) error {
	info, err := ktavArtifactStatFile(sourcePath)
	if err != nil {
		return fmt.Errorf("ktav artifact migration: cannot stat %s to preserve its mtime: %w", filepath.Base(sourcePath), err)
	}
	mod := info.ModTime()
	if err := ktavArtifactChtimes(ktavPath, mod, mod); err != nil {
		return fmt.Errorf("ktav artifact migration: cannot preserve the mtime of %s: %w", filepath.Base(ktavPath), err)
	}
	return nil
}

// ktavMigrateOAuthJournalsAtStartup converts every rotating provider's legacy
// JSON journal for the given config slots BEFORE any recovery worker starts,
// and finishes any leftover journal transaction (e.g. a post-archive crash
// that left the record beside the Ktav and backup). It is gated on
// ktavAutoMigrate and never fails a load: any error defers the conversion
// with a once-per-path, paths-only warning.
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
			if err := ktavMigrateOAuthJournal(ctx, canonical, descriptor.id, workingDir); err != nil {
				if errors.Is(err, errKtavMigrationConflict) && ktavEntryExists(oauthJournalKtavPath(canonical, descriptor.id)) && ktavEntryExists(legacy) {
					warnKtavArtifactConflictOnce(oauthJournalKtavPath(canonical, descriptor.id), legacy, err.Error())
				} else {
					warnMigrationDeferredOnce(legacy, err.Error())
				}
			}
		}
	}
}

// ktavValidateOAuthJournalBytes re-validates raw journal bytes as an OAuth
// record. It runs INSIDE the refresh lock on the exact bytes the converter
// would convert, closing the validate-then-convert TOCTOU window.
func ktavValidateOAuthJournalBytes(raw []byte) error {
	var record oauthJournalRecord
	if json.Unmarshal(raw, &record) != nil || !validOAuthJournal(&record) {
		return errOAuthJournalInvalid
	}
	return nil
}

// ktavMigrateOAuthJournal converts one provider's pending journal under the
// SAME legacy-anchored refresh sidecar lock the recovery worker uses. A
// journal that is absent, unreadable, or invalid stays exactly where it is:
// invalid journals belong to the recovery worker's quarantine path. When the
// JSON is already gone but a leftover transaction remains (record, or Ktav
// and backup), the state machine still runs so the record is retired and no
// bytes are stranded.
func ktavMigrateOAuthJournal(ctx context.Context, configPath, providerID, workingDir string) error {
	legacy := oauthJournalLegacyPath(configPath, providerID)
	dest := oauthJournalKtavPath(configPath, providerID)
	if ktavEntryExists(legacy) {
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
	} else if !ktavEntryExists(migrationRecordPathFor(legacy)) && !ktavEntryExists(dest) {
		// Absent JSON without any leftover transaction: nothing to recover
		// and nothing to convert — never an error.
		return nil
	}
	if err := migrateKtavArtifactToWithLock(ctx, legacy, dest, workingDir, oauthRefreshLockPath(configPath, providerID), ktavValidateOAuthJournalBytes); err != nil {
		return err
	}
	backup := backupPathFor(legacy)
	if !ktavEntryExists(backup) {
		// Nothing was archived (e.g. the journal was invalidated under the
		// lock and left for quarantine).
		return nil
	}
	// Prove the converted journal still decodes to the identical JSON-shaped
	// record: the retained backup and the PUBLISHED journal must be
	// Equivalent at the exact path selectOAuthJournalPath resolves.
	backupBytes, err := os.ReadFile(backup)
	if err != nil {
		return fmt.Errorf("ktav journal migration: cannot read backup %s: %w", filepath.Base(backup), err)
	}
	sourceTree, err := ktavcodec.ParseJSONLossless(backupBytes)
	if err != nil {
		return fmt.Errorf("ktav journal migration: backup %s is not the original record: %w", filepath.Base(backup), err)
	}
	ktavRaw, err := os.ReadFile(dest)
	if err != nil {
		return fmt.Errorf("ktav journal migration: cannot read %s: %w", filepath.Base(dest), err)
	}
	ktavTree, err := ktavcodec.DecodeKtav(ktavRaw)
	if err != nil {
		return fmt.Errorf("ktav journal migration: invalid Ktav in %s: %w", filepath.Base(dest), err)
	}
	if err := ktavcodec.Equivalent(sourceTree, ktavTree); err != nil {
		return fmt.Errorf("ktav journal migration: %s does not preserve the original record: %w", filepath.Base(dest), err)
	}
	return nil
}

// warnKtavArtifactConflictOnce logs an artifact migration that stopped on a
// conflict beside an EXISTING destination Ktav: the existing Ktav generation
// stays active and the JSON copy is left untouched. It keys off the shared
// ktavBothFormatsWarned map (distinct prefix) so tests can reset it, once
// per destination, paths only.
func warnKtavArtifactConflictOnce(ktav, legacy, reason string) {
	if _, dup := ktavBothFormatsWarned.LoadOrStore("conflict:"+ktav, struct{}{}); dup {
		return
	}
	slog.Warn("Ktav artifact migration stopped; an existing Ktav generation stays active and the JSON copy is left untouched",
		"ktav", ktav, "json", legacy, "reason", reason)
}

// ktavMigrateCacheArtifactsStartup converts the provider/hyper catalogs and
// every model-catalog cache in the data directory, and finishes any pending
// cache transaction. It runs INDEPENDENTLY of the workspace config format:
// an already-migrated (rush.ktav) or absent workspace must still convert its
// caches and recover their pending records. Gated on ktavAutoMigrate;
// failures defer with a paths-only warning.
func ktavMigrateCacheArtifactsStartup(ctx context.Context, workingDir string) {
	if !ktavAutoMigrate {
		return
	}
	for _, legacy := range ktavCacheArtifactPaths() {
		if !ktavEntryExists(legacy) && !ktavEntryExists(migrationRecordPathFor(legacy)) && !ktavEntryExists(ktavSiblingOf(legacy)) {
			continue
		}
		if err := migrateKtavArtifact(ctx, legacy, workingDir); err != nil {
			if errors.Is(err, errKtavMigrationConflict) && ktavEntryExists(ktavSiblingOf(legacy)) && ktavEntryExists(legacy) {
				warnKtavArtifactConflictOnce(ktavSiblingOf(legacy), legacy, err.Error())
			} else {
				warnMigrationDeferredOnce(legacy, err.Error())
			}
		}
	}
}

// ktavCacheArtifactPaths lists the Rush-owned cache slots by their legacy
// JSON paths: providers.json, hyper.json, model-catalog-<id>.json. Pending
// post-archive transactions (record but no JSON) are ALSO discovered through
// their <name>.json.ktav-migration records, so a catalog whose legacy JSON is
// already archived is still recovered.
func ktavCacheArtifactPaths() []string {
	providers := cachePathFor("providers")
	cacheDir := filepath.Dir(providers)
	paths := []string{providers, cachePathFor("hyper")}
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	for _, pattern := range []string{"model-catalog-*.json", "model-catalog-*.json.ktav-migration"} {
		matches, err := filepath.Glob(filepath.Join(cacheDir, pattern))
		if err != nil {
			continue
		}
		for _, match := range matches {
			legacy := strings.TrimSuffix(match, ".ktav-migration")
			if !seen[legacy] {
				seen[legacy] = true
				paths = append(paths, legacy)
			}
		}
	}
	return paths
}
