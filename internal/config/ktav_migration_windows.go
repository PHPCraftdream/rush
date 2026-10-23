//go:build windows

package config

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// archiveKtavBackup renames the legacy JSON to its retained backup name
// with a no-replace rename issued THROUGH the pinned parent handle, after
// re-proving the source identity and bytes through that same parent. The
// source handle is opened via the parent and stays open across validation
// and the rename, so a path substitution cannot redirect either step.
func archiveKtavBackup(ctx context.Context, sourcePath, backupPath string, expected reloadFileFingerprint, owner int, enforceOwner bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, err := openWindowsConfigParent(sourcePath)
	if err != nil {
		return fmt.Errorf("open config parent: %w", err)
	}
	defer parent.Close()

	parentInfo, err := parent.Stat()
	if err != nil {
		return err
	}
	parentOwner, ownerKnown := configFileOwner(parentInfo)
	if enforceOwner && (!ownerKnown || parentOwner != owner) {
		return errConfigCommitVerification
	}
	if !expected.parentIdentity.valid || configFileIdentityOfOpened(parent, parentInfo) != expected.parentIdentity {
		return errConfigCommitVerification
	}
	backupBase := filepath.Base(backupPath)
	backupHandle, inspectErr := openWindowsConfigEntryAt(parent, backupBase)
	if inspectErr == nil {
		_ = backupHandle.Close()
		return fmt.Errorf("%w: backup %s already exists", errKtavMigrationConflict, backupBase)
	}
	if !isWindowsEntryNotFound(inspectErr) && !os.IsNotExist(inspectErr) {
		return fmt.Errorf("inspect backup %s: %v", backupBase, inspectErr)
	}

	sourceBase := filepath.Base(sourcePath)
	handle, err := createWindowsConfigEntryAt(parent, sourceBase, windows.GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.DELETE|windows.SYNCHRONIZE, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if err != nil {
		return fmt.Errorf("%w: open source: %v", errConfigCommitVerification, err)
	}
	source := os.NewFile(uintptr(handle), filepath.Join(parent.Name(), sourceBase))
	defer source.Close()
	data, actual, err := readStableConfigHandle(source, owner, enforceOwner)
	if err != nil {
		return err
	}
	if actual.digest != expected.digest || !sameBytesFingerprint(data, expected.digest) ||
		!actual.identity.valid || !expected.identity.valid || actual.identity != expected.identity || actual.nlink != 1 {
		return fmt.Errorf("%w: config source changed during migration", errConfigCommitVerification)
	}
	if err := runKtavMigrationStep("during-backup-rename"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := renameWindowsConfigHandleNoReplace(source, parent, backupBase); err != nil {
		return err
	}
	if err := runKtavMigrationStep("after-backup-rename"); err != nil {
		return newCommitOutcome(backupPath, true, false, errAtomicWriteCommitted, err)
	}
	archived, archivedFP, err := readStableConfigHandle(source, owner, enforceOwner)
	if err != nil {
		return fmt.Errorf("%w: %s or %s cannot be verified after archival: %v", errKtavMigrationConflict, filepath.Base(sourcePath), backupBase, err)
	}
	if archivedFP.digest != expected.digest || !sameBytesFingerprint(archived, expected.digest) ||
		!archivedFP.identity.valid || archivedFP.identity != expected.identity || archivedFP.nlink != 1 ||
		(enforceOwner && archivedFP.owner != owner) {
		return fmt.Errorf("%w: %s or %s changed during archival", errKtavMigrationConflict, filepath.Base(sourcePath), backupBase)
	}
	if err := syncConfigParentFile(parent); err != nil {
		return newCommitOutcome(backupPath, true, false, errAtomicWriteCommitted, errConfigCommitDurabilityUncertain, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// renameWindowsConfigHandleNoReplace issues FileRenameInformationEx with
// POSIX semantics (no replacement) against the pinned parent directory
// handle, for an existing source handle. FILE_RENAME_POSIX_SEMANTICS alone
// (without REPLACE_IF_EXISTS) fails with a collision status when the
// destination exists: the operation is genuinely no-replace.
func renameWindowsConfigHandleNoReplace(source, parent *os.File, destination string) error {
	name, err := windows.UTF16FromString(destination)
	if err != nil {
		return err
	}
	name = name[:len(name)-1]
	headerSize := int(unsafe.Offsetof(windowsFileRenameInfo{}.FileName))
	buffer := make([]byte, headerSize+len(name)*2)
	info := (*windowsFileRenameInfo)(unsafe.Pointer(&buffer[0]))
	info.Flags = windows.FILE_RENAME_POSIX_SEMANTICS
	info.RootDirectory = windows.Handle(parent.Fd())
	info.FileNameLength = uint32(len(name) * 2)
	for i, value := range name {
		binary.LittleEndian.PutUint16(buffer[headerSize+i*2:], value)
	}
	return windows.SetFileInformationByHandle(
		windows.Handle(source.Fd()),
		windows.FileRenameInfoEx,
		&buffer[0],
		uint32(len(buffer)),
	)
}
