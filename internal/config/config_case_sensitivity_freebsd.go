//go:build freebsd

package config

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

func configPlatformCaseFoldLeaf(dir, leaf string) (string, bool) {
	return configFoldCaseLeaf(leaf, configPlatformCaseSensitivity(dir))
}

func configPlatformCaseSensitivity(dir string) configCaseSensitivity {
	return configFreeBSDDirectoryCaseSensitivity(dir)
}

func configFreeBSDDirectoryCaseSensitivity(dir string) configCaseSensitivity {
	path, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return configCaseSensitivityUnknown
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return configCaseSensitivityUnknown
	}
	defer unix.Close(fd)

	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(fd, &filesystem); err != nil {
		return configCaseSensitivityUnknown
	}
	filesystemName := filesystem.Fstypename[:]
	for i, char := range filesystemName {
		if char == 0 {
			filesystemName = filesystemName[:i]
			break
		}
	}
	sensitivity := configFreeBSDCaseSensitivityFromFilesystemName(string(filesystemName))
	if sensitivity == configCaseSensitivityUnknown {
		sensitivity = configDirectoryCaseSensitivityByEntries(dir)
	}
	return sensitivity
}
