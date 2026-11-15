//go:build windows

package fsext

import (
	"cmp"
	"os"
	"path/filepath"
)

// WindowsWorkingDirDrive returns the drive letter of the current working
// directory, e.g. "C:".
// Falls back to the system drive if the current working directory cannot be
// determined.
func WindowsWorkingDirDrive() string {
	if cwd, err := os.Getwd(); err == nil {
		return filepath.VolumeName(cwd)
	}
	return WindowsSystemDrive()
}

// WindowsSystemDrive returns the drive letter of the system drive, e.g. "C:".
func WindowsSystemDrive() string {
	systemRoot := cmp.Or(os.Getenv("SYSTEMROOT"), os.Getenv("WINDIR"))
	return filepath.VolumeName(systemRoot)
}

// Owner retrieves the user ID of the owner of the file or directory at the
// specified path.
func Owner(path string) (int, error) {
	_, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return -1, nil
}
