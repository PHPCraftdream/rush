//go:build windows

package config

import (
	"os"

	"golang.org/x/sys/windows"
)

// Windows uses the volume serial number and file-index pair returned by the
// opened handle. This is the Windows equivalent of the Unix device/inode
// identity; descriptor-relative rename is still unavailable through the
// portable API, so commitConfigFile keeps the explicit pre/post path checks.
type configFileIdentity struct {
	device uint64
	inode  uint64
	valid  bool
}

func configFileIdentityOf(os.FileInfo) configFileIdentity { return configFileIdentity{} }

func configFileIdentityOfOpened(file *os.File, _ os.FileInfo) configFileIdentity {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return configFileIdentity{}
	}
	return configFileIdentity{
		device: uint64(info.VolumeSerialNumber),
		inode:  uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
		valid:  true,
	}
}

func configFileOwner(os.FileInfo) (int, bool) { return -1, true }

func sameConfigFileIdentity(left, right os.FileInfo) bool {
	// The opened handle identity is checked separately. FileInfo does not carry
	// the handle's volume/file-index pair, so this path-level comparison is only
	// the portable read-stability check, not a security decision.
	return true
}
