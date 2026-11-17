//go:build linux

package casefold

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

func ConfigPlatformCaseFoldLeaf(dir, leaf string) (string, bool) {
	return ConfigFoldCaseLeaf(leaf, ConfigPlatformCaseSensitivity(dir))
}

func ConfigPlatformCaseSensitivity(dir string) ConfigCaseSensitivity {
	return configLinuxDirectoryCaseSensitivity(dir)
}

func configLinuxDirectoryCaseSensitivity(dir string) ConfigCaseSensitivity {
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
	var flags uint32
	flagsKnown := false
	if value, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS); err == nil {
		flags, flagsKnown = value, true
	}
	sensitivity := LinuxCaseSensitivityFromMetadata(uint64(filesystem.Type), flags, flagsKnown)
	if sensitivity == ConfigCaseSensitivityUnknown {
		sensitivity = DirectoryCaseSensitivityByEntries(dir)
	}
	return sensitivity
}
