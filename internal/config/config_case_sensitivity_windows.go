//go:build windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// configPlatformCaseFoldLeaf uses the directory's read-only Win32 case flag.
// Older filesystems without this information class retain Windows' historical
// case-insensitive default; ambiguous errors fail closed.
func configPlatformCaseFoldLeaf(dir, leaf string) (string, bool) {
	return configFoldCaseLeaf(leaf, configPlatformCaseSensitivity(dir))
}

func configPlatformCaseSensitivity(dir string) configCaseSensitivity {
	caseInsensitive, err := windowsConfigDirectoryCaseInsensitive(dir)
	if err != nil {
		return configCaseSensitivityUnknown
	}
	if caseInsensitive {
		return configCaseSensitivityInsensitive
	}
	return configCaseSensitivitySensitive
}

func windowsConfigDirectoryCaseInsensitive(path string) (bool, error) {
	directory, err := openWindowsConfigHandle(filepath.Clean(path), windows.GENERIC_READ|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		return false, err
	}
	defer directory.Close()
	info, err := windowsFileInformation(directory)
	if err != nil {
		return false, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false, os.ErrInvalid
	}
	var flags uint32
	err = windows.GetFileInformationByHandleEx(
		windows.Handle(directory.Fd()), windows.FileCaseSensitiveInfo,
		(*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)),
	)
	if err != nil {
		if windowsCaseSensitivityInfoUnsupported(err) {
			return true, nil
		}
		return false, err
	}
	return configWindowsCaseInsensitiveFromFlags(flags), nil
}

func configWindowsCaseInsensitiveFromFlags(flags uint32) bool {
	return flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR == 0
}

func windowsCaseSensitivityInfoUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED)
}
