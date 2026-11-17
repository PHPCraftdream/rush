//go:build darwin

package casefold

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Darwin's _PC_CASE_SENSITIVE selector from sys/unistd.h.
const darwinPathconfCaseSensitive = 11

// ConfigPlatformCaseFoldLeaf reads filesystem case sensitivity without
// creating a directory entry. Unknown capability results fail closed.
func ConfigPlatformCaseFoldLeaf(dir, leaf string) (string, bool) {
	return ConfigFoldCaseLeaf(leaf, ConfigPlatformCaseSensitivity(dir))
}

func ConfigPlatformCaseSensitivity(dir string) ConfigCaseSensitivity {
	path, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return ConfigCaseSensitivityUnknown
	}
	value, err := unix.Pathconf(path, darwinPathconfCaseSensitive)
	return DarwinCaseSensitivityFromPathconf(value, err)
}
