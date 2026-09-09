//go:build linux

package config

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

func configPlatformCaseFoldLeaf(dir, leaf string) (string, bool) {
	return configFoldCaseLeaf(leaf, configPlatformCaseSensitivity(dir))
}

func configPlatformCaseSensitivity(dir string) configCaseSensitivity {
	return configLinuxDirectoryCaseSensitivity(dir)
}

func configLinuxDirectoryCaseSensitivity(dir string) configCaseSensitivity {
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
	var flags uint32
	flagsKnown := false
	if value, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS); err == nil {
		flags, flagsKnown = value, true
	}
	sensitivity := configLinuxCaseSensitivityFromMetadata(uint64(filesystem.Type), flags, flagsKnown)
	if sensitivity == configCaseSensitivityUnknown {
		sensitivity = configDirectoryCaseSensitivityByEntries(dir)
	}
	return sensitivity
}
