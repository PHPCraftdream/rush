//go:build !windows

package config

import (
	"os"

	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fswap"
)

// io layer: fswap. Unix-only seam; RenameConfigTempAt has no windows build.
func renameConfigTempAt(oldDirFD int, oldName string, newDirFD int, newName string, replace bool) (bool, error) {
	return fswap.RenameConfigTempAt(oldDirFD, oldName, newDirFD, newName, replace)
}

// io layer: fileid. Unix-only seam; config_commit_unix_test.go has no windows build.
func configFileIdentityOf(info os.FileInfo) fileid.FileIdentity {
	return fileid.FileIdentityOf(info)
}

// io layer: fileid. Unix-only seam; config_rename_unix_test.go has no windows build.
func configFileNlinkOfOpened(file *os.File, info os.FileInfo) uint64 {
	return fileid.FileNlinkOfOpened(file, info)
}
