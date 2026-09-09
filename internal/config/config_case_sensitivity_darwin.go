//go:build darwin

package config

import (
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	darwinVolumeCapabilityAttributeSize = configVolumeCapabilityAttributeSize
)

// configPlatformCaseFoldLeaf reads APFS/HFS volume capabilities without
// creating a directory entry. Unknown capability results fail closed.
func configPlatformCaseFoldLeaf(dir, leaf string) (string, bool) {
	return configFoldCaseLeaf(leaf, configPlatformCaseSensitivity(dir))
}

func configPlatformCaseSensitivity(dir string) configCaseSensitivity {
	path, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return configCaseSensitivityUnknown
	}
	pathPtr, err := unix.BytePtrFromString(path)
	if err != nil {
		return configCaseSensitivityUnknown
	}
	attrs := unix.Attrlist{Bitmapcount: 5, Volattr: unix.ATTR_VOL_INFO | unix.ATTR_VOL_CAPABILITIES}
	buffer := make([]byte, darwinVolumeCapabilityAttributeSize)
	_, _, errno := unix.Syscall6(
		unix.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&attrs)),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
		0,
		0,
	)
	if errno != 0 {
		return configCaseSensitivityUnknown
	}
	caseInsensitive, known := configParseDarwinVolumeCapabilities(buffer)
	if !known {
		return configCaseSensitivityUnknown
	}
	if caseInsensitive {
		return configCaseSensitivityInsensitive
	}
	return configCaseSensitivitySensitive
}
