//go:build windows

package config

import "os"

// Windows policy: owner and opened-file identity are unavailable through the
// portable Go file API used here. The Windows path policy therefore does not
// claim Unix-grade ownership or symlink-ABA guarantees; callers still read
// from one opened handle and retain the explicit -1 owner bypass.
type configFileIdentity struct {
	device uint64
	inode  uint64
	valid  bool
}

func configFileIdentityOf(os.FileInfo) configFileIdentity { return configFileIdentity{} }

func configFileOwner(os.FileInfo) (int, bool) { return -1, true }

func sameConfigFileIdentity(left, right os.FileInfo) bool {
	// The Windows policy cannot verify a stable physical identity. The caller
	// must not interpret this as a security check.
	return true
}
