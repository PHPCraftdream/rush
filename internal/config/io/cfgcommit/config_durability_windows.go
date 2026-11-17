//go:build windows

package cfgcommit

import (
	"os"

	"golang.org/x/sys/windows"

	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
)

// Windows has no portable directory fsync equivalent. Flush the opened
// directory when the filesystem accepts it; an error is meaningful because
// namespace durability cannot otherwise be confirmed.
func syncConfigParentOnDisk(path string) error {
	parent, err := fileid.OpenWindowsConfigHandle(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
