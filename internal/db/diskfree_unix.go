//go:build !windows

package db

// Free-disk-space reporting for the compact command's space reservation
// check (task #1161). Unix implementation: statfs.

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// DiskFreeBytes reports the available and total bytes on the volume
// holding path. Path must exist.
func DiskFreeBytes(path string) (available uint64, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("disk free: %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), nil
}
