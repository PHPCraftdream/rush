//go:build !windows

package config

import (
	"os"
	"syscall"
)

// configFileIdentity is the platform-neutral portion of an opened-file
// identity. Unix can verify it; Windows deliberately reports it unavailable
// rather than pretending that a path check is an equivalent security proof.
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

func configFileOwner(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}

func sameConfigFileIdentity(left, right os.FileInfo) bool {
	a := configFileIdentityOf(left)
	b := configFileIdentityOf(right)
	return a.valid && b.valid && a == b
}
