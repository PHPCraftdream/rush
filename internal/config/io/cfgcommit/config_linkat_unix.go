//go:build !windows

package cfgcommit

import (
	"os"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fswap"
)

// io layer: fswap.
func recoverConfigTempAlias(parent *os.File, base string, expected docread.Fingerprint, enforceOwner bool, expectedOwner int) (docread.Fingerprint, error) {
	return fswap.RecoverConfigTempAlias(parent, base, expected, enforceOwner, expectedOwner)
}

// io layer: fswap.
func cleanupConfigTempAlias(dirFD int, name string, expected fileid.FileIdentity) error {
	return fswap.CleanupConfigTempAlias(dirFD, name, expected)
}

// io layer: fswap.
func unlinkConfigTempAt(dirFD int, name string) error {
	return fswap.UnlinkConfigTempAt(dirFD, name)
}
