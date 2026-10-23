//go:build !windows

package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// archiveKtavBackup renames the legacy JSON to its retained backup name
// using an identity-checked, no-replace operation through the pinned parent
// descriptor. A pre-existing backup is never replaced.
func archiveKtavBackup(ctx context.Context, sourcePath, backupPath string, expected reloadFileFingerprint, owner int, enforceOwner bool) error {
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
	parentOwner, ownerKnown := configFileOwner(parentInfo)
	if enforceOwner && (!ownerKnown || parentOwner != owner) {
		return errConfigCommitVerification
	}
	actualParent := configFileIdentityOf(parentInfo)
	if !actualParent.valid || actualParent != expected.parentIdentity {
		return errConfigCommitVerification
	}
	backupBase := filepath.Base(backupPath)
	backupFD, backupErr := unix.Openat(parentFD, backupBase, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if backupErr == nil {
		_ = unix.Close(backupFD)
		return fmt.Errorf("%w: backup %s already exists", errKtavMigrationConflict, backupBase)
	}
	if backupErr != unix.ENOENT {
		return fmt.Errorf("inspect backup %s: %w", backupBase, backupErr)
	}

	// Prove the pinned directory still holds exactly the generation that
	// was converted, through the same descriptor the rename will use.
	data, actual, err := readConfigEntryAt(parentFD, filepath.Base(sourcePath), owner, enforceOwner)
	if err != nil {
		return err
	}
	if actual.digest != expected.digest || !sameBytesFingerprint(data, expected.digest) || actual.identity != expected.identity || actual.nlink != 1 || (enforceOwner && actual.owner != owner) {
		return fmt.Errorf("%w: config source changed during migration", errConfigCommitVerification)
	}
	if err := runKtavMigrationStep("during-backup-rename"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	published, err := renameConfigTempAt(parentFD, filepath.Base(sourcePath), parentFD, backupBase, false)
	if err != nil {
		if published {
			// The archival happened even though the API reported an error;
			// never retry a mutation that may already be visible.
			return newCommitOutcome(backupPath, true, false, errAtomicWriteCommitted, errConfigCommitDurabilityUncertain, err)
		}
		return err
	}
	if err := runKtavMigrationStep("after-backup-rename"); err != nil {
		return newCommitOutcome(backupPath, true, false, errAtomicWriteCommitted, err)
	}
	if err := syncConfigParentFile(parent); err != nil {
		return newCommitOutcome(backupPath, true, false, errAtomicWriteCommitted, errConfigCommitDurabilityUncertain, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
