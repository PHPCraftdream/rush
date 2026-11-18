//go:build linux

package fswap

import (
	"errors"

	"golang.org/x/sys/unix"

	"github.com/PHPCraftdream/rush/internal/config/io/cfghooks"
)

func RenameConfigTempAt(oldDirFD int, oldName string, newDirFD int, newName string, replace bool) (bool, error) {
	if replace {
		err := unix.Renameat(oldDirFD, oldName, newDirFD, newName)
		return err == nil, err
	}
	hooks := cfghooks.Snapshot()
	if hooks.ForceLinkNoReplace {
		return PublishConfigTempNoReplaceAt(oldDirFD, oldName, newDirFD, newName)
	}
	renameNoReplace := hooks.RenameNoReplace
	var err error
	if renameNoReplace != nil {
		err = renameNoReplace(oldDirFD, oldName, newDirFD, newName)
	} else {
		err = unix.Renameat2(oldDirFD, oldName, newDirFD, newName, unix.RENAME_NOREPLACE)
	}
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
		return PublishConfigTempNoReplaceAt(oldDirFD, oldName, newDirFD, newName)
	}
	return err == nil, err
}
