//go:build !windows

package slotmigrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"golang.org/x/sys/unix"
)

// archiveKtavBackup renames the legacy JSON to its retained backup name
// using an identity-checked, no-replace operation through the pinned parent
// descriptor. A pre-existing backup is never replaced.
func archiveKtavBackup(ctx context.Context, sourcePath, backupPath string, expected docread.Fingerprint, owner int, enforceOwner bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parentPath := filepath.Dir(sourcePath)
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open config parent: %w", err)
	}
	parent := os.NewFile(uintptr(parentFD), parentPath)
	defer parent.Close()

	parentInfo, err := parent.Stat()
	if err != nil {
		return err
	}
	parentOwner, ownerKnown := fileid.ConfigFileOwner(parentInfo)
	if enforceOwner && (!ownerKnown || parentOwner != owner) {
		return fileid.ErrConfigCommitVerification()
	}
	actualParent := fileid.FileIdentityOf(parentInfo)
	if !actualParent.Valid || actualParent != expected.ParentIdentity {
		return fileid.ErrConfigCommitVerification()
	}
	backupBase := filepath.Base(backupPath)
	backupFD, backupErr := unix.Openat(parentFD, backupBase, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if backupErr == nil {
		_ = unix.Close(backupFD)
		return fmt.Errorf("%w: backup %s already exists", docread.ErrKtavMigrationConflict(), backupBase)
	}
	if backupErr != unix.ENOENT {
		return fmt.Errorf("inspect backup %s: %w", backupBase, backupErr)
	}

	// Prove the pinned directory still holds exactly the generation that
	// was converted, through the same descriptor the rename will use.
	data, actual, err := docread.ReadConfigEntryAt(parentFD, filepath.Base(sourcePath), owner, enforceOwner)
	if err != nil {
		return err
	}
	if actual.Digest != expected.Digest || !docread.SameBytesFingerprint(data, expected.Digest) || actual.Identity != expected.Identity || actual.Nlink != 1 || (enforceOwner && actual.Owner != owner) {
		return fmt.Errorf("%w: config source changed during migration", fileid.ErrConfigCommitVerification())
	}
	if err := runKtavMigrationStep("during-backup-rename"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	published, err := cfgcommit.RenameConfigTempAt(parentFD, filepath.Base(sourcePath), parentFD, backupBase, false)
	if err != nil {
		if published {
			// The archival happened even though the API reported an error;
			// never retry a mutation that may already be visible.
			return cfgcommit.NewCommitOutcome(backupPath, true, false, cfgcommit.ErrAtomicWriteCommitted(), cfgcommit.ErrConfigCommitDurabilityUncertain(), err)
		}
		return err
	}
	if err := runKtavMigrationStep("after-backup-rename"); err != nil {
		return cfgcommit.NewCommitOutcome(backupPath, true, false, cfgcommit.ErrAtomicWriteCommitted(), err)
	}
	archived, archivedFP, err := docread.ReadConfigEntryAt(parentFD, backupBase, owner, enforceOwner)
	if err != nil {
		return fmt.Errorf("%w: %s or %s cannot be verified after archival: %v", docread.ErrKtavMigrationConflict(), filepath.Base(sourcePath), backupBase, err)
	}
	if archivedFP.Digest != expected.Digest || !docread.SameBytesFingerprint(archived, expected.Digest) ||
		archivedFP.Identity != expected.Identity || archivedFP.Nlink != 1 ||
		(enforceOwner && archivedFP.Owner != owner) {
		return fmt.Errorf("%w: %s or %s changed during archival", docread.ErrKtavMigrationConflict(), filepath.Base(sourcePath), backupBase)
	}
	if err := cfgcommit.SyncConfigParentFile(parent); err != nil {
		return cfgcommit.NewCommitOutcome(backupPath, true, false, cfgcommit.ErrAtomicWriteCommitted(), cfgcommit.ErrConfigCommitDurabilityUncertain(), err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
