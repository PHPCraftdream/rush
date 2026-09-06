//go:build darwin

package config

import (
	"errors"

	"golang.org/x/sys/unix"
)

// Darwin exposes an atomic no-replace rename through renameatx_np. Passing the
// already-open transaction directories keeps both operands descriptor-relative.
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
	configTestHooks.Lock()
	renameatxNp := configTestHooks.renameatxNp
	configTestHooks.Unlock()
	if renameatxNp == nil {
		renameatxNp = unix.RenameatxNp
	}
	err := renameatxNp(oldDirFD, oldName, newDirFD, newName, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		return publishConfigTempNoReplaceAt(oldDirFD, oldName, newDirFD, newName)
	}
	return err == nil, err
}
