// Ktav migration for symlinked legacy configs: dotfiles managers keep
// rush.json as a symbolic link into a versioned repository. The physical
// target is the backup, so the migration publishes rush.ktav beside the
// LINK path and leaves both the link and its target untouched.
package slotmigrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
	"github.com/PHPCraftdream/rush/internal/kit/text/ktavcodec"
)

const ktavSymlinkDeferredReason = "ktav migration: legacy config is a symbolic link whose target is not a regular file or is unreadable"

func isKtavSymlinkPath(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// runKtavSymlinkMigrationState routes symlink-specific migration states.
// It returns handled=false whenever the on-disk state is not a symlink
// migration matter, so the generic machine keeps its own recovery and
// conflict semantics.
func runKtavSymlinkMigrationState(ctx context.Context, legacy, recordPath, workingDir string, state ktavMigrationEntries) (bool, error) {
	ktavPath := docread.KtavSiblingOf(legacy)
	if state.recordExists {
		record, err := loadKtavMigrationRecord(recordPath, legacy, ktavPath, workingDir)
		if err != nil {
			// Foreign or malformed record: the generic machine owns
			// its recovery and conflict reporting.
			return false, nil
		}
		if !record.Symlink {
			return false, nil
		}
		if !state.ktavExists {
			return true, recoverKtavSymlinkBeforePublication(ctx, legacy, recordPath, ktavPath, record, workingDir)
		}
		return true, finishKtavSymlinkMigration(ctx, legacy, recordPath, ktavPath, record, workingDir)
	}
	if state.jsonExists && !state.ktavExists && !state.recordExists && !state.backupExists && isKtavSymlinkPath(legacy) {
		return true, migrateKtavSymlinkedConfig(ctx, legacy, recordPath, workingDir)
	}
	return false, nil
}

// recoverKtavSymlinkBeforePublication rebuilds the staged document from the
// still-authoritative symlink source after a crash between record commit and
// destination publication.
func recoverKtavSymlinkBeforePublication(ctx context.Context, legacy, recordPath, ktavPath string, record *KtavMigrationRecord, workingDir string) error {
	owner, enforce, err := OwnerPolicyForMigration(legacy, workingDir)
	if err != nil {
		return err
	}
	source, fp, err := docread.ReadStableConfigFileOwned(legacy, owner, enforce)
	if err != nil {
		return fmt.Errorf("%w: cannot read symlinked source %s: %v", docread.ErrKtavMigrationConflict(), filepath.Base(legacy), err)
	}
	var want [sha256.Size]byte
	if _, err := hex.Decode(want[:], []byte(record.SourceDigest)); err != nil || fp.Digest != want || !docread.SameBytesFingerprint(source, want) || fmt.Sprintf("%v", fp.Identity) != record.SourceID || fmt.Sprintf("%v", fp.ParentIdentity) != record.ParentID {
		return fmt.Errorf("%w: migration record does not match source %s", docread.ErrKtavMigrationConflict(), filepath.Base(legacy))
	}
	if err := ktavMigrationCheckContext(ctx, "before-convert"); err != nil {
		return err
	}
	data, err := ktavcodec.ConvertJSONToKtav(source)
	if err != nil {
		return fmt.Errorf("ktav conversion failed for %s: %w", filepath.Base(legacy), err)
	}
	data, err = ApplyKtavMigrationModelsIntegrity(source, data)
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(data)) != record.StagedDigest {
		return fmt.Errorf("%w: regenerated destination does not match migration record", docread.ErrKtavMigrationConflict())
	}
	_, expected, err := docread.ReadStableConfigFileOwned(ktavPath, owner, enforce)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if expected.Exists {
		return fmt.Errorf("%w: %s appeared during recovery", docread.ErrKtavMigrationConflict(), filepath.Base(ktavPath))
	}
	if _, err := cfgcommit.CommitConfigFile(ktavPath, ktavPath, data, 0o600, expected, owner, enforce); err != nil {
		return fmt.Errorf("ktav migration: failed to publish %s: %w", ktavPath, err)
	}
	return finishKtavSymlinkMigration(ctx, legacy, recordPath, ktavPath, record, workingDir)
}

// finishKtavSymlinkMigration recovers a symlink migration whose record
// (Symlink: true) and published rush.ktav already exist: verify the
// digests, retire the record, and log once; the link is kept. Crash points: record + ktav + link, or record +
// ktav with the link already removed.
func finishKtavSymlinkMigration(ctx context.Context, legacy, recordPath, ktavPath string, record *KtavMigrationRecord, workingDir string) error {
	owner, enforce, err := OwnerPolicyForMigration(legacy, workingDir)
	if err != nil {
		return err
	}
	ktavData, ktavFP, err := docread.ReadStableConfigFileOwned(ktavPath, owner, enforce)
	if err != nil {
		return fmt.Errorf("%w: cannot verify published Ktav %s: %v", docread.ErrKtavMigrationRecord(), filepath.Base(ktavPath), err)
	}
	var staged [sha256.Size]byte
	if _, err := hex.Decode(staged[:], []byte(record.StagedDigest)); err != nil {
		return fmt.Errorf("%w: staged digest is malformed", docread.ErrKtavMigrationRecord())
	}
	if ktavFP.Digest != staged || !docread.SameBytesFingerprint(ktavData, staged) {
		return fmt.Errorf("%w: migration record does not match published %s", docread.ErrKtavMigrationRecord(), filepath.Base(ktavPath))
	}
	if isKtavSymlinkPath(legacy) {
		data, fp, err := docread.ReadStableConfigFileOwned(legacy, owner, enforce)
		if err != nil {
			return fmt.Errorf("%w: cannot read symlinked source %s: %v", docread.ErrKtavMigrationConflict(), filepath.Base(legacy), err)
		}
		var want [sha256.Size]byte
		if _, err := hex.Decode(want[:], []byte(record.SourceDigest)); err != nil || fp.Digest != want || !docread.SameBytesFingerprint(data, want) || fmt.Sprintf("%v", fp.Identity) != record.SourceID || fmt.Sprintf("%v", fp.ParentIdentity) != record.ParentID {
			return fmt.Errorf("%w: migration record does not match source %s", docread.ErrKtavMigrationConflict(), filepath.Base(legacy))
		}
		// The link is kept, like a regular legacy JSON, for older binaries.
	}
	if err := removeKtavMigrationRecord(recordPath); err != nil {
		return err
	}
	slog.Info("Ktav migration: legacy symlinked config migrated; the symbolic link and its target were left untouched", "ktav", ktavPath, "link", legacy)
	return nil
}

// migrateKtavSymlinkedConfig performs a fresh migration of a symlinked
// legacy config. No .bak is ever created: the physical link target in the
// dotfiles repository IS the backup.
func migrateKtavSymlinkedConfig(ctx context.Context, legacy, recordPath, workingDir string) error {
	owner, enforce, err := OwnerPolicyForMigration(legacy, workingDir)
	if err != nil {
		return err
	}
	info, err := os.Stat(legacy)
	if err != nil || !info.Mode().IsRegular() {
		docread.WarnMigrationDeferredOnce(legacy, ktavSymlinkDeferredReason)
		return nil
	}
	source, sourceFP, err := docread.ReadStableConfigFileOwned(legacy, owner, enforce)
	if err != nil {
		docread.WarnMigrationDeferredOnce(legacy, ktavSymlinkDeferredReason)
		return nil
	}
	if sourceFP.Nlink != 1 {
		return fileid.ErrConfigHardLink
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
	stagedDigest := sha256.Sum256(ktavBytes)
	ktavPath := docread.KtavSiblingOf(legacy)
	record := KtavMigrationRecord{
		Version:      KtavMigrationRecordVersion,
		Source:       filepath.Base(legacy),
		Destination:  filepath.Base(ktavPath),
		Backup:       filepath.Base(docread.BackupPathFor(legacy)),
		SourceDigest: hex.EncodeToString(sourceFP.Digest[:]),
		StagedDigest: hex.EncodeToString(stagedDigest[:]),
		SourceID:     fmt.Sprintf("%v", sourceFP.Identity),
		ParentID:     fmt.Sprintf("%v", sourceFP.ParentIdentity),
		Symlink:      true,
	}
	recordBytes, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("ktav migration: failed to encode record: %w", err)
	}
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
		if step == "after-staging" {
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
		}
		return nil
	}
	if err := ktavMigrationCheckContext(ctx, "before-ktav-rename"); err != nil {
		return err
	}
	// Publish rush.ktav as a regular file beside the LINK path; the
	// physical link target is never written to or removed here.
	if _, err := cfgcommit.CommitConfigFile(ktavPath, ktavPath, ktavBytes, 0o600, expected, owner, enforce, publishHook); err != nil {
		return fmt.Errorf("ktav migration: failed to publish %s: %w", ktavPath, err)
	}
	if err := ktavMigrationCheckContext(ctx, "after-publish"); err != nil {
		return err
	}
	slog.Info("Ktav migration: legacy symlinked config migrated; the symbolic link and its target were left untouched", "ktav", ktavPath, "link", legacy)
	return removeKtavMigrationRecord(recordPath)
}
