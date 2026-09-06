//go:build !windows

package config

import "golang.org/x/sys/unix"

// publishConfigTempNoReplaceAt links the fully-written temporary inode into
// the destination name and then removes the temporary name. linkat is
// atomic with respect to an existing destination: it fails with EEXIST and
// never replaces that entry. The returned bool records whether publication
// happened before a cleanup error.
func publishConfigTempNoReplaceAt(oldDirFD int, oldName string, newDirFD int, newName string) (bool, error) {
	if err := unix.Linkat(oldDirFD, oldName, newDirFD, newName, 0); err != nil {
		return false, err
	}
	if err := unlinkConfigTempAt(oldDirFD, oldName); err != nil {
		return true, err
	}
	return true, nil
}

func unlinkConfigTempAt(dirFD int, name string) error {
	configTestHooks.Lock()
	hook := configTestHooks.unlinkTemp
	configTestHooks.Unlock()
	if hook != nil {
		return hook(dirFD, name)
	}
	return unix.Unlinkat(dirFD, name, 0)
}
