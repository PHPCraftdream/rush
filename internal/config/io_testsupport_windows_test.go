//go:build windows

package config

import (
	"os"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/casefold"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fswap"
)

// io layer: fileid. Windows-only seam.
func deleteWindowsConfigHandle(file *os.File) error {
	return fileid.DeleteWindowsConfigHandle(file)
}

// io layer: fileid. Windows-only seam.
func windowsConfigAPIPath(path string) (string, error) {
	return fileid.WindowsConfigAPIPath(path)
}

// io layer: fileid. Windows-only seam.
func stageConfigFileHandle(path string, data []byte, perm os.FileMode) (fileid.WindowsStagedConfigFile, error) {
	return fileid.StageConfigFileHandle(path, data, perm)
}

// io layer: fileid. Windows-only seam.
func openWindowsConfigHandle(path string, access, flags uint32) (*os.File, error) {
	return fileid.OpenWindowsConfigHandle(path, access, flags)
}

// io layer: fswap. Windows-only seam.
type windowsFileRenameInfo = fswap.WindowsFileRenameInfo

// io layer: fileid. Windows-only seam.
func configFileIdentityAtPath(path string) (fileid.FileIdentity, error) {
	return fileid.FileIdentityAtPath(path)
}

// io layer: fswap. Windows-only seam.
func renameConfigTemp(source, destination string, replace bool) error {
	return fswap.RenameConfigTemp(source, destination, replace)
}

// io layer: fswap. Windows-only seam.
func renameConfigTempHandle(source *os.File, sourcePath string, parent *os.File, destination string, replace bool, expected docread.Fingerprint, expectedOwner int, enforceOwner bool, discoveryPath string) error {
	return fswap.RenameConfigTempHandle(source, sourcePath, parent, destination, replace, expected, expectedOwner, enforceOwner, discoveryPath)
}

// io layer: casefold. Windows-only seam.
func configWindowsCaseInsensitiveFromFlags(flags uint32) bool {
	return casefold.WindowsCaseInsensitiveFromFlags(flags)
}
