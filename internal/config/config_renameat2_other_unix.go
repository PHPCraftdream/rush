//go:build !windows && !linux

package config

import "golang.org/x/sys/unix"

func renameConfigTempAt(oldDirFD int, oldName string, newDirFD int, newName string, replace bool) (bool, error) {
	if replace {
		err := unix.Renameat(oldDirFD, oldName, newDirFD, newName)
		return err == nil, err
	}
	// linkat publishes the already-synced temporary inode only when the
	// destination is absent. It is the portable Unix equivalent of
	// RENAME_NOREPLACE; unlinking the temporary spelling then leaves the new
	// destination with exactly one link.
	return publishConfigTempNoReplaceAt(oldDirFD, oldName, newDirFD, newName)
}
