//go:build !windows

package docread

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
	"golang.org/x/sys/unix"
)

// Fingerprint captures the stable file metadata needed by Unix config commits.
// Discovery fields are carried through alias recovery by the caller.
type Fingerprint struct {
	Exists          bool
	Size            int64
	ModTime         int64
	Digest          [sha256.Size]byte
	Owner           int
	Nlink           uint64
	Identity        fileid.FileIdentity
	Discovery       [sha256.Size]byte
	ParentDiscovery [sha256.Size]byte
}

// ReadConfigEntryAt opens a directory entry without following symlinks and
// returns bytes observed identically twice alongside their descriptor-bound
// fingerprint.
func ReadConfigEntryAt(dirFD int, name string, expectedOwner int, enforceOwner bool) ([]byte, Fingerprint, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, Fingerprint{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, Fingerprint{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, Fingerprint{}, fmt.Errorf("%w: config entry is not a regular file", fileid.ErrConfigCommitVerification())
	}
	owner, ownerKnown := fileid.ConfigFileOwner(info)
	if enforceOwner && (!ownerKnown || owner != expectedOwner) {
		return nil, Fingerprint{}, fileid.ErrConfigOwnerMismatch()
	}
	first, err := fileid.ReadOpenedConfigBytes(file)
	if err != nil {
		return nil, Fingerprint{}, err
	}
	second, err := fileid.ReadOpenedConfigBytes(file)
	if err != nil {
		return nil, Fingerprint{}, err
	}
	if !bytes.Equal(first, second) {
		return nil, Fingerprint{}, fileid.ErrStableReadUnstable()
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return nil, Fingerprint{}, err
	}
	if !fileid.SameConfigFileIdentity(info, finalInfo) {
		return nil, Fingerprint{}, fileid.ErrStableReadUnstable()
	}
	identity := fileid.FileIdentityOf(finalInfo)
	return second, Fingerprint{
		Exists: true, Size: int64(len(second)), ModTime: finalInfo.ModTime().UnixNano(),
		Digest: sha256.Sum256(second), Owner: owner, Nlink: fileid.FileNlinkOfInfo(finalInfo),
		Identity: identity,
	}, nil
}
