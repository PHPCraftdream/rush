//go:build !windows

package fsext

import (
	"os"
	"syscall"
)

// WindowsWorkingDirDrive returns the drive letter of the current working
// directory, e.g. "C:".
// Falls back to the system drive if the current working directory cannot be
// determined.
func WindowsWorkingDirDrive() string {
	panic("cannot call fsext.WindowsWorkingDirDrive() on non-Windows OS")
}

// WindowsSystemDrive returns the drive letter of the system drive, e.g. "C:".
func WindowsSystemDrive() string {
	panic("cannot call fsext.WindowsSystemDrive() on non-Windows OS")
}

// Owner retrieves the user ID of the owner of the file or directory at the
// specified path.
func Owner(path string) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	var uid int
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		uid = int(stat.Uid)
	} else {
		uid = os.Getuid()
	}
	return uid, nil
}
