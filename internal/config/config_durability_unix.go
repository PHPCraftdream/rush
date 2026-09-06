//go:build !windows

package config

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncConfigParentOnDisk persists the directory entry update made by rename.
func syncConfigParentOnDisk(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), path)
	defer dir.Close()
	return dir.Sync()
}
