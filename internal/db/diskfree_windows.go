//go:build windows

package db

// Free-disk-space reporting for the compact command's space reservation
// check (task #1161). Windows implementation: GetDiskFreeSpaceEx.

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// DiskFreeBytes reports the available and total bytes on the volume
// holding path. Path must exist.
func DiskFreeBytes(path string) (available uint64, total uint64, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, 0, fmt.Errorf("disk free: resolve %s: %w", path, err)
	}
	ptr, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return 0, 0, fmt.Errorf("disk free: %s: %w", abs, err)
	}
	var freeToCaller, totalBytes, freeAvailable uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, &freeToCaller, &totalBytes, &freeAvailable); err != nil {
		return 0, 0, fmt.Errorf("disk free: %s: %w", abs, err)
	}
	// freeToCaller honours per-user quotas (freeAvailable is volume-wide);
	// it is the Windows counterpart of the Unix variant's Bavail.
	return freeToCaller, totalBytes, nil
}
