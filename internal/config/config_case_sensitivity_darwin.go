//go:build darwin

package config

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Darwin's _PC_CASE_SENSITIVE selector from sys/unistd.h.
const darwinPathconfCaseSensitive = 11

// configPlatformCaseFoldLeaf reads filesystem case sensitivity without
// creating a directory entry. Unknown capability results fail closed.
func configPlatformCaseFoldLeaf(dir, leaf string) (string, bool) {
	return configFoldCaseLeaf(leaf, configPlatformCaseSensitivity(dir))
}

func configPlatformCaseSensitivity(dir string) configCaseSensitivity {
	path, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return configCaseSensitivityUnknown
	}
	value, err := unix.Pathconf(path, darwinPathconfCaseSensitive)
	return configDarwinCaseSensitivityFromPathconf(value, err)
}
