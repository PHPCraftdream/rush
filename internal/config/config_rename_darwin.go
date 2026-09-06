//go:build darwin

package config

import (
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// Darwin exposes an atomic no-replace rename through renamex_np. /dev/fd
// keeps both operands rooted at the already-open transaction directories.
func renameConfigTempAt(oldDirFD int, oldName string, newDirFD int, newName string, replace bool) (bool, error) {
	if replace {
		err := unix.Renameat(oldDirFD, oldName, newDirFD, newName)
		return err == nil, err
	}
	configTestHooks.Lock()
	forceLink := configTestHooks.forceLinkNoReplace
	configTestHooks.Unlock()
	if forceLink {
		return publishConfigTempNoReplaceAt(oldDirFD, oldName, newDirFD, newName)
	}
	if _, nlink, err := configTempIdentityAt(oldDirFD, oldName); err != nil {
		return false, err
	} else if nlink != 1 {
		return false, ErrConfigHardLink
	}
	oldPath := filepath.Join("/dev/fd", strconv.Itoa(oldDirFD), oldName)
	newPath := filepath.Join("/dev/fd", strconv.Itoa(newDirFD), newName)
	err := unix.RenamexNp(oldPath, newPath, unix.RENAME_EXCL)
	return err == nil, err
}
