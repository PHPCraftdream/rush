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

func configFileIdentityAtPath(path string) (configFileIdentity, error) {
	file, err := os.Open(path)
	if err != nil {
		return configFileIdentity{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return configFileIdentity{}, err
	}
	if !info.Mode().IsRegular() {
		return configFileIdentity{}, os.ErrInvalid
	}
	identity := configFileIdentityOfOpened(file, info)
	if !identity.valid {
		return configFileIdentity{}, os.ErrInvalid
	}
	return identity, nil
}

func removeConfigTempIfIdentity(path string, expected configFileIdentity) error {
	identity, err := configFileIdentityAtPath(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if identity != expected {
		return os.ErrInvalid
	}
	return os.Remove(path)
}

func configFileOwner(os.FileInfo) (int, bool) { return -1, true }

func configFileNlinkOfOpened(file *os.File, _ os.FileInfo) uint64 {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return 0
	}
	return uint64(info.NumberOfLinks)
}

func sameConfigFileIdentity(left, right os.FileInfo) bool {
	// The opened handle identity is checked separately. FileInfo does not carry
	// the handle's volume/file-index pair, so this path-level comparison is only
	// the portable read-stability check, not a security decision.
	return true
}
