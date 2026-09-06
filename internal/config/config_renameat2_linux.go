//go:build linux

package config

import "golang.org/x/sys/unix"

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
	err := unix.Renameat2(oldDirFD, oldName, newDirFD, newName, unix.RENAME_NOREPLACE)
	return err == nil, err
}
