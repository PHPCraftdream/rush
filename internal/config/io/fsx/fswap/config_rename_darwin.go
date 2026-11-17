//go:build darwin

package fswap

import (
	"errors"

	"golang.org/x/sys/unix"

	"github.com/PHPCraftdream/rush/internal/config/io/cfghooks"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
)

// Darwin exposes an atomic no-replace rename through renameatx_np. Passing the
// already-open transaction directories keeps both operands descriptor-relative.
func RenameConfigTempAt(oldDirFD int, oldName string, newDirFD int, newName string, replace bool) (bool, error) {
	if replace {
		err := unix.Renameat(oldDirFD, oldName, newDirFD, newName)
		return err == nil, err
	}
	hooks := cfghooks.Snapshot()
	if hooks.ForceLinkNoReplace {
		return PublishConfigTempNoReplaceAt(oldDirFD, oldName, newDirFD, newName)
	}
	if _, nlink, err := ConfigTempIdentityAt(oldDirFD, oldName); err != nil {
		return false, err
	} else if nlink != 1 {
		return false, fileid.ErrConfigHardLink
	}
	renameatxNp := cfghooks.Snapshot().RenameatxNp
	if renameatxNp == nil {
		renameatxNp = unix.RenameatxNp
	}
	err := renameatxNp(oldDirFD, oldName, newDirFD, newName, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		return PublishConfigTempNoReplaceAt(oldDirFD, oldName, newDirFD, newName)
	}
	return err == nil, err
}
