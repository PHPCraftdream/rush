//go:build windows

package docread

import (
	"bytes"
	"crypto/sha256"
	"os"

	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"golang.org/x/sys/windows"
)

func ReadStableConfigHandle(file *os.File, expectedOwner int, enforceOwner bool) ([]byte, Fingerprint, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, Fingerprint{}, err
	}
	handleInfo, err := fileid.WindowsFileInformation(file)
	if err != nil {
		return nil, Fingerprint{}, err
	}
	if !info.Mode().IsRegular() || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, Fingerprint{}, os.ErrInvalid
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
	return second, Fingerprint{
		Exists: true, Size: int64(len(second)), ModTime: finalInfo.ModTime().UnixNano(),
		Digest: sha256.Sum256(second), Owner: owner,
		Nlink:    fileid.FileNlinkOfOpened(file, finalInfo),
		Identity: fileid.FileIdentityOfOpened(file, finalInfo),
	}, nil
}
