//go:build !windows

package config

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// publishConfigTempNoReplaceAt links the fully-written temporary inode into
// the destination name and then removes the temporary name. linkat is
// atomic with respect to an existing destination: it fails with EEXIST and
// never replaces that entry. The returned bool records whether publication
// happened before a cleanup error.
func publishConfigTempNoReplaceAt(oldDirFD int, oldName string, newDirFD int, newName string) (bool, error) {
	tempIdentity, tempNlink, err := configTempIdentityAt(oldDirFD, oldName)
	if err != nil {
		return false, err
	}
	if tempNlink != 1 {
		return false, ErrConfigHardLink
	}
	if err := unix.Linkat(oldDirFD, oldName, newDirFD, newName, 0); err != nil {
		return false, err
	}
	linkedIdentity, linkedNlink, err := configTempIdentityAt(oldDirFD, oldName)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return true, err
	}
	if linkedIdentity != tempIdentity {
		return true, fmt.Errorf("%w: temporary config alias changed", errConfigCommitVerification)
	}
	if linkedNlink > 2 {
		return true, ErrConfigHardLink
	}
	if linkedNlink != 2 {
		return true, fmt.Errorf("%w: temporary config alias link count changed", errConfigCommitVerification)
	}
	if err := unlinkConfigTempAt(oldDirFD, oldName); err != nil {
		return true, err
	}
	return true, nil
}

func configTempIdentityAt(dirFD int, name string) (configFileIdentity, uint64, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return configFileIdentity{}, 0, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return configFileIdentity{}, 0, err
	}
	if !info.Mode().IsRegular() {
		return configFileIdentity{}, 0, fmt.Errorf("%w: temporary config is not a regular file", errConfigCommitVerification)
	}
	identity := configFileIdentityOf(info)
	if !identity.valid {
		return configFileIdentity{}, 0, fmt.Errorf("%w: temporary config identity unavailable", errConfigCommitVerification)
	}
	return identity, configFileNlinkOfOpened(file, info), nil
}

func cleanupConfigTempAlias(dirFD int, name string, expected configFileIdentity) error {
	identity, _, err := configTempIdentityAt(dirFD, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if identity != expected {
		return fmt.Errorf("%w: temporary config alias changed", errConfigCommitVerification)
	}
	return unlinkConfigTempAt(dirFD, name)
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
