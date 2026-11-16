//go:build freebsd

package casefold

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

func ConfigPlatformCaseFoldLeaf(dir, leaf string) (string, bool) {
	return ConfigFoldCaseLeaf(leaf, ConfigPlatformCaseSensitivity(dir))
}

func ConfigPlatformCaseSensitivity(dir string) ConfigCaseSensitivity {
	return configFreeBSDDirectoryCaseSensitivity(dir)
}

func configFreeBSDDirectoryCaseSensitivity(dir string) ConfigCaseSensitivity {
	path, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return ConfigCaseSensitivityUnknown
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return ConfigCaseSensitivityUnknown
	}
	defer unix.Close(fd)

	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(fd, &filesystem); err != nil {
		return ConfigCaseSensitivityUnknown
	}
	filesystemName := filesystem.Fstypename[:]
	for i, char := range filesystemName {
		if char == 0 {
			filesystemName = filesystemName[:i]
			break
		}
	}
	sensitivity := FreeBSDCaseSensitivityFromFilesystemName(string(filesystemName))
	if sensitivity == ConfigCaseSensitivityUnknown {
		sensitivity = DirectoryCaseSensitivityByEntries(dir)
	}
	return sensitivity
}
