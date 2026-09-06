//go:build !windows

package config

import (
	"os"
	"syscall"
)

// configFileIdentity is the platform-neutral portion of an opened-file
// identity. Unix derives it from the opened file's stat information; Windows
// derives the equivalent identity from its handle API.
type configFileIdentity struct {
	device uint64
	inode  uint64
	valid  bool
}

func configFileIdentityOf(info os.FileInfo) configFileIdentity {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return configFileIdentity{}
	}
	return configFileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino), valid: true}
}

func configFileIdentityOfOpened(_ *os.File, info os.FileInfo) configFileIdentity {
	return configFileIdentityOf(info)
}

func configFileOwner(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}

func configFileNlinkOfOpened(_ *os.File, info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}

func sameConfigFileIdentity(left, right os.FileInfo) bool {
	a := configFileIdentityOf(left)
	b := configFileIdentityOf(right)
	return a.valid && b.valid && a == b
}

func configFilePathIdentityMatches(path string, _ *os.File, openedInfo os.FileInfo) (bool, error) {
	pathInfo, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if pathInfo.IsDir() {
		return false, nil
	}
	return sameConfigFileIdentity(openedInfo, pathInfo), nil
}
