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

func renameConfigTempHandle(source *os.File, sourcePath string, parent *os.File, destination string, replace bool) error {
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
	err = setInfo(windows.FileRenameInfoEx, buffer)
	if err == nil || (err != windows.ERROR_INVALID_PARAMETER && err != windows.ERROR_INVALID_FUNCTION && err != windows.ERROR_NOT_SUPPORTED) {
		return err
	}
	legacyBuffer := append([]byte(nil), buffer...)
	legacyInfo := (*windowsFileRenameInfo)(unsafe.Pointer(&legacyBuffer[0]))
	if replace {
		legacyInfo.Flags = 1
	} else {
		legacyInfo.Flags = 0
	}
	err = setInfo(windows.FileRenameInfo, legacyBuffer)
	if err == nil || !isUnsupportedRenameError(err) || hasSetFileInformationHook() {
		return err
	}
	var status windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(
		windows.Handle(source.Fd()),
		&status,
		&legacyBuffer[0],
		uint32(len(legacyBuffer)),
		windows.FileRenameInformation,
	)
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
