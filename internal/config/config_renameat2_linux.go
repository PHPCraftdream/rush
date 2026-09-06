//go:build linux

package config

import (
	"errors"

	"golang.org/x/sys/unix"
)

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
	configTestHooks.Lock()
	renameNoReplace := configTestHooks.renameNoReplace
	configTestHooks.Unlock()
	var err error
	if renameNoReplace != nil {
		err = renameNoReplace(oldDirFD, oldName, newDirFD, newName)
	} else {
		err = unix.Renameat2(oldDirFD, oldName, newDirFD, newName, unix.RENAME_NOREPLACE)
	}
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
		return publishConfigTempNoReplaceAt(oldDirFD, oldName, newDirFD, newName)
	}
	return err == nil, err
}
