//go:build !windows

package fileid

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// configFileIdentity is the platform-neutral portion of an opened-file
// identity. Unix derives it from the opened file's stat information; Windows
// derives the equivalent identity from its handle API.
type FileIdentity struct {
	device uint64
	inode  uint64
	Valid  bool
}

func (identity FileIdentity) FingerprintString() string {
	return fmt.Sprintf("%t|%d|%d", identity.Valid, identity.device, identity.inode)
}

func FileIdentityOf(info os.FileInfo) FileIdentity {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}
	}
	return FileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino), Valid: true}
}

func FileIdentityOfOpened(_ *os.File, info os.FileInfo) FileIdentity {
	return FileIdentityOf(info)
}

func FileIdentityAtPath(path string) (FileIdentity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return FileIdentity{}, err
	}
	if !info.Mode().IsRegular() {
		return FileIdentity{}, os.ErrInvalid
	}
	identity := FileIdentityOf(info)
	if !identity.Valid {
		return FileIdentity{}, os.ErrInvalid
	}
	return identity, nil
}

func ConfigFileOwner(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}

func FileNlinkOfInfo(info os.FileInfo) uint64 {
	return FileNlinkOfOpened(nil, info)
}

func FileNlinkOfOpened(_ *os.File, info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}

func SameConfigFileIdentity(left, right os.FileInfo) bool {
	a := FileIdentityOf(left)
	b := FileIdentityOf(right)
	return a.Valid && b.Valid && a == b
}

func ConfigFilePathIdentityMatches(path string, _ *os.File, openedInfo os.FileInfo) (bool, error) {
	pathInfo, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if pathInfo.IsDir() {
		return false, nil
	}
	return SameConfigFileIdentity(openedInfo, pathInfo), nil
}

// homeConfigOwner is the explicit trust policy for user-level config files.
// It must not inherit the owner of the checkout that happens to be loaded.
func HomeConfigOwner() int { return os.Getuid() }

// systemConfigOwner is the explicit trust policy for the system config.
func SystemConfigOwner() int { return 0 }

func ConfigParentIdentity(path string) FileIdentity {
	info, err := os.Stat(filepath.Dir(filepath.Clean(path)))
	if err != nil {
		return FileIdentity{}
	}
	return FileIdentityOf(info)
}

// OpenStableConfigFile opens a regular config file without blocking on a FIFO.
// The opened descriptor is stat-checked before callers read from it.
func OpenStableConfigFile(path string) (*os.File, error) {
	// O_NONBLOCK makes opening a FIFO independent of a writer. The descriptor
	// is still fstat-checked by readStableConfigFileOwned before any read.
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return nil, fmt.Errorf("%w: %s", ErrConfigNonRegular, path)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, os.ErrInvalid
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s", ErrConfigNonRegular, path)
	}
	return file, nil
}

// WriteStableConfigFile writes config data with owner-only permissions.
func WriteStableConfigFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
