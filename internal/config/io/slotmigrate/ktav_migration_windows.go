//go:build windows

package slotmigrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
	"github.com/PHPCraftdream/rush/internal/config/io/fswap"
	"golang.org/x/sys/windows"
)

// archiveKtavBackup renames the legacy JSON to its retained backup name
// with a no-replace rename issued THROUGH the pinned parent handle, after
// re-proving the source identity and bytes through that same parent. The
// source handle is opened via the parent and stays open across validation
// and the rename, so a path substitution cannot redirect either step.
func archiveKtavBackup(ctx context.Context, sourcePath, backupPath string, expected docread.Fingerprint, owner int, enforceOwner bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, err := fileid.OpenWindowsConfigParent(sourcePath)
	if err != nil {
		return fmt.Errorf("open config parent: %w", err)
	}
	defer parent.Close()

	parentInfo, err := parent.Stat()
	if err != nil {
		return err
	}
	parentOwner, ownerKnown := fileid.ConfigFileOwner(parentInfo)
	if enforceOwner && (!ownerKnown || parentOwner != owner) {
		return fileid.ErrConfigCommitVerification()
	}
	if !expected.ParentIdentity.Valid || fileid.FileIdentityOfOpened(parent, parentInfo) != expected.ParentIdentity {
		return fileid.ErrConfigCommitVerification()
	}
	backupBase := filepath.Base(backupPath)
	backupHandle, inspectErr := fileid.OpenWindowsConfigEntryAt(parent, backupBase)
	if inspectErr == nil {
		_ = backupHandle.Close()
		return fmt.Errorf("%w: backup %s already exists", docread.ErrKtavMigrationConflict(), backupBase)
	}
	if !fileid.IsWindowsEntryNotFound(inspectErr) && !os.IsNotExist(inspectErr) {
		return fmt.Errorf("inspect backup %s: %v", backupBase, inspectErr)
	}

	sourceBase := filepath.Base(sourcePath)
	handle, err := fileid.CreateWindowsConfigEntryAt(parent, sourceBase, windows.GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.DELETE|windows.SYNCHRONIZE, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if err != nil {
		return fmt.Errorf("%w: open source: %v", fileid.ErrConfigCommitVerification(), err)
	}
	source := os.NewFile(uintptr(handle), filepath.Join(parent.Name(), sourceBase))
	defer source.Close()
	data, actual, err := docread.ReadStableConfigHandle(source, owner, enforceOwner)
	if err != nil {
		return err
	}
	if actual.Digest != expected.Digest || !docread.SameBytesFingerprint(data, expected.Digest) ||
		!actual.Identity.Valid || !expected.Identity.Valid || actual.Identity != expected.Identity || actual.Nlink != 1 {
		return fmt.Errorf("%w: config source changed during migration", fileid.ErrConfigCommitVerification())
	}
	if err := runKtavMigrationStep("during-backup-rename"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fswap.RenameConfigTempHandle(source, sourcePath, parent, backupBase, false, expected, owner, enforceOwner, sourcePath); err != nil {
		return err
	}
	if err := runKtavMigrationStep("after-backup-rename"); err != nil {
		return cfgcommit.NewCommitOutcome(backupPath, true, false, cfgcommit.ErrAtomicWriteCommitted(), err)
	}
	archived, archivedFP, err := docread.ReadStableConfigHandle(source, owner, enforceOwner)
	if err != nil {
		return fmt.Errorf("%w: %s or %s cannot be verified after archival: %v", docread.ErrKtavMigrationConflict(), filepath.Base(sourcePath), backupBase, err)
	}
	if archivedFP.Digest != expected.Digest || !docread.SameBytesFingerprint(archived, expected.Digest) ||
		!archivedFP.Identity.Valid || archivedFP.Identity != expected.Identity || archivedFP.Nlink != 1 ||
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
