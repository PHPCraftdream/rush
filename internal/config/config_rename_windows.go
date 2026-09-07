//go:build windows

package config

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsFileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

func renameConfigTempHandle(source *os.File, sourcePath string, parent *os.File, destination string, replace bool, expected reloadFileFingerprint, expectedOwner int, enforceOwner bool, discoveryPath string) error {
	// Keep the old seam for existing deterministic tests. Production has no
	// MoveFileEx hook and therefore always takes the handle-based path below.
	configTestHooks.Lock()
	moveFileEx := configTestHooks.moveFileEx
	configTestHooks.Unlock()
	if moveFileEx != nil {
		from, err := windows.UTF16PtrFromString(sourcePath)
		if err != nil {
			return err
		}
		to, err := windows.UTF16PtrFromString(filepath.Join(filepath.Dir(sourcePath), destination))
		if err != nil {
			return err
		}
		flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
		if replace {
			flags |= windows.MOVEFILE_REPLACE_EXISTING
		}
		return moveFileEx(from, to, flags)
	}

	name, err := windows.UTF16FromString(destination)
	if err != nil {
		return err
	}
	name = name[:len(name)-1]
	headerSize := int(unsafe.Offsetof(windowsFileRenameInfo{}.FileName))
	buffer := make([]byte, headerSize+len(name)*2)
	info := (*windowsFileRenameInfo)(unsafe.Pointer(&buffer[0]))
	info.Flags = windows.FILE_RENAME_POSIX_SEMANTICS
	if replace {
		info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS
		info.Flags |= windows.FILE_RENAME_POSIX_SEMANTICS
	}
	// Resolve the relative destination against the already-open parent handle.
	// This is the publication boundary: a later pathname substitution cannot
	// redirect the rename to a replacement directory.
	info.RootDirectory = windows.Handle(parent.Fd())
	info.FileNameLength = uint32(len(name) * 2)
	for i, value := range name {
		binary.LittleEndian.PutUint16(buffer[headerSize+i*2:], value)
	}

	setInfo := func(class uint32, payload []byte) error {
		configTestHooks.Lock()
		hook := configTestHooks.setFileInformation
		configTestHooks.Unlock()
		if hook != nil {
			return hook(uintptr(source.Fd()), class, &payload[0], uint32(len(payload)))
		}
		return windows.SetFileInformationByHandle(windows.Handle(source.Fd()), class, &payload[0], uint32(len(payload)))
	}
	for attempt := 0; ; attempt++ {
		err = setInfo(windows.FileRenameInfoEx, buffer)
		if err == nil || (err != windows.ERROR_INVALID_PARAMETER && err != windows.ERROR_INVALID_FUNCTION && err != windows.ERROR_NOT_SUPPORTED) {
			if err == nil {
				return nil
			}
			if attempt+1 >= windowsRenameRetryAttempts || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				return err
			}
			if retryErr := waitAndVerifyWindowsRenameRetry(source, sourcePath, parent, destination, replace, expected, expectedOwner, enforceOwner, discoveryPath, attempt); retryErr != nil {
				return errors.Join(err, retryErr)
			}
			continue
		}
		legacyBuffer := append([]byte(nil), buffer...)
		legacyInfo := (*windowsFileRenameInfo)(unsafe.Pointer(&legacyBuffer[0]))
		if replace {
			legacyInfo.Flags = 1
		} else {
			legacyInfo.Flags = 0
		}
		if retryErr := verifyWindowsRenameRetryState(source, sourcePath, parent, destination, replace, expected, expectedOwner, enforceOwner, discoveryPath); retryErr != nil {
			return errors.Join(err, retryErr)
		}
		err = setInfo(windows.FileRenameInfo, legacyBuffer)
		if err == nil || !isUnsupportedRenameError(err) || hasSetFileInformationHook() {
			if err == nil {
				return nil
			}
			if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				return err
			}
			if attempt+1 >= windowsRenameRetryAttempts {
				return err
			}
			if retryErr := waitAndVerifyWindowsRenameRetry(source, sourcePath, parent, destination, replace, expected, expectedOwner, enforceOwner, discoveryPath, attempt); retryErr != nil {
				return errors.Join(err, retryErr)
			}
			continue
		}
		if retryErr := verifyWindowsRenameRetryState(source, sourcePath, parent, destination, replace, expected, expectedOwner, enforceOwner, discoveryPath); retryErr != nil {
			return errors.Join(err, retryErr)
		}
		var status windows.IO_STATUS_BLOCK
		err = windows.NtSetInformationFile(
			windows.Handle(source.Fd()),
			&status,
			&legacyBuffer[0],
			uint32(len(legacyBuffer)),
			windows.FileRenameInformation,
		)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || attempt+1 >= windowsRenameRetryAttempts {
			return err
		}
		if retryErr := waitAndVerifyWindowsRenameRetry(source, sourcePath, parent, destination, replace, expected, expectedOwner, enforceOwner, discoveryPath, attempt); retryErr != nil {
			return errors.Join(err, retryErr)
		}
	}
}

const windowsRenameRetryAttempts = 8

func waitAndVerifyWindowsRenameRetry(source *os.File, sourcePath string, parent *os.File, destination string, replace bool, expected reloadFileFingerprint, expectedOwner int, enforceOwner bool, discoveryPath string, attempt int) error {
	time.Sleep(time.Duration(attempt+1) * 2 * time.Millisecond)
	return verifyWindowsRenameRetryState(source, sourcePath, parent, destination, replace, expected, expectedOwner, enforceOwner, discoveryPath)
}

func verifyWindowsRenameRetryState(source *os.File, sourcePath string, parent *os.File, destination string, replace bool, expected reloadFileFingerprint, expectedOwner int, enforceOwner bool, discoveryPath string) error {
	// Validate after backoff while the publication handle remains pinned.
	stagedIdentity := configFileIdentityOfOpened(source, nil)
	if !stagedIdentity.valid {
		return errConfigCommitVerification
	}
	if expected.parentIdentity.valid && configFileIdentityOfOpened(parent, nil) != expected.parentIdentity {
		return errConfigCommitVerification
	}
	if expected.parentDiscovery != ([32]byte{}) && configDiscoveryFingerprint(filepath.Dir(discoveryPath)) != expected.parentDiscovery {
		return errConfigCommitVerification
	}
	staged, err := openWindowsConfigEntryAt(parent, filepath.Base(sourcePath))
	if err != nil {
		return errConfigCommitVerification
	}
	defer staged.Close()
	stagedInfo, err := staged.Stat()
	if err != nil || stagedInfo.IsDir() {
		return errConfigCommitVerification
	}
	stagedHandleInfo, err := windowsFileInformation(staged)
	if err != nil || stagedHandleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || configFileIdentityOfOpened(staged, stagedInfo) != stagedIdentity {
		return errConfigCommitVerification
	}

	target, err := openWindowsConfigEntryAt(parent, filepath.Base(destination))
	if err == nil {
		defer target.Close()
	}
	if !replace {
		if isWindowsEntryNotFound(err) || os.IsNotExist(err) {
			return nil
		}
		return errConfigCommitVerification
	}
	if err != nil {
		return errConfigCommitVerification
	}
	targetInfo, err := target.Stat()
	if err != nil || targetInfo.IsDir() {
		return errConfigCommitVerification
	}
	targetHandleInfo, err := windowsFileInformation(target)
	if err != nil || targetHandleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errConfigCommitVerification
	}
	targetData, targetFingerprint, err := readStableConfigHandle(target, expectedOwner, enforceOwner)
	if err != nil || !expected.exists || !expected.identity.valid || targetFingerprint.exists != expected.exists ||
		targetFingerprint.size != expected.size || targetFingerprint.modTime != expected.modTime ||
		targetFingerprint.digest != expected.digest || targetFingerprint.owner != expected.owner ||
		targetFingerprint.nlink != expected.nlink || targetFingerprint.identity != expected.identity ||
		!sameBytesFingerprint(targetData, expected.digest) {
		return errConfigCommitVerification
	}
	if expected.discovery != ([32]byte{}) && configDiscoveryFingerprint(discoveryPath) != expected.discovery {
		return errConfigCommitVerification
	}
	return nil
}

func isUnsupportedRenameError(err error) bool {
	return err == windows.ERROR_INVALID_PARAMETER || err == windows.ERROR_INVALID_FUNCTION || err == windows.ERROR_NOT_SUPPORTED
}

func hasSetFileInformationHook() bool {
	configTestHooks.Lock()
	hook := configTestHooks.setFileInformation
	configTestHooks.Unlock()
	return hook != nil
}

// renameConfigTemp uses MoveFileEx directly so the expected-absent case is a
// true no-replace operation. os.Rename on Windows is intentionally not used:
// its implementation requests replacement semantics.
func renameConfigTemp(source, destination string, replace bool) error {
	apiSource, err := windowsConfigAPIPath(source)
	if err != nil {
		return fmt.Errorf("encode temporary config path: %w", err)
	}
	apiDestination, err := windowsConfigAPIPath(destination)
	if err != nil {
		return fmt.Errorf("encode config path: %w", err)
	}
	from, err := windows.UTF16PtrFromString(apiSource)
	if err != nil {
		return fmt.Errorf("encode temporary config path: %w", err)
	}
	to, err := windows.UTF16PtrFromString(apiDestination)
	if err != nil {
		return fmt.Errorf("encode config path: %w", err)
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if replace {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	for attempt := 0; ; attempt++ {
		configTestHooks.Lock()
		moveFileEx := configTestHooks.moveFileEx
		configTestHooks.Unlock()
		if moveFileEx != nil {
			err = moveFileEx(from, to, flags)
		} else {
			err = windows.MoveFileEx(from, to, flags)
		}
		if err == nil || !replace || !errors.Is(err, windows.ERROR_ACCESS_DENIED) || attempt == 7 {
			return err
		}
		// Do not retry an access error after the source has disappeared. That
		// is the observable signature of a publication whose WRITE_THROUGH
		// result was lost; retrying would replace the original error with a
		// misleading second failure and discard the durability signal.
		if _, statErr := os.Stat(source); os.IsNotExist(statErr) {
			return err
		}
		// A cooperating writer can still be completing its pre-lock handle
		// close because target resolution deliberately precedes lock acquire.
		// Give that short-lived reader time to release FILE_SHARE_DELETE before
		// treating the replacement as a real access failure.
		time.Sleep(2 * time.Millisecond)
	}
}
