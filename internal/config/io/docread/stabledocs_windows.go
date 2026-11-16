//go:build windows

package docread

import (
	"bytes"
	"crypto/sha256"
	"os"

	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
	"golang.org/x/sys/windows"
)

func readStableConfigHandle(file *os.File, expectedOwner int, enforceOwner bool) ([]byte, reloadFileFingerprint, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	handleInfo, err := fileid.WindowsFileInformation(file)
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	if !info.Mode().IsRegular() || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, reloadFileFingerprint{}, os.ErrInvalid
	}
	owner, ownerKnown := fileid.ConfigFileOwner(info)
	if enforceOwner && (!ownerKnown || owner != expectedOwner) {
		return nil, reloadFileFingerprint{}, fileid.ErrConfigOwnerMismatch()
	}
	first, err := fileid.ReadOpenedConfigBytes(file)
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	second, err := fileid.ReadOpenedConfigBytes(file)
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	if !bytes.Equal(first, second) {
		return nil, reloadFileFingerprint{}, fileid.ErrStableReadUnstable()
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	return second, reloadFileFingerprint{
		exists: true, size: int64(len(second)), modTime: finalInfo.ModTime().UnixNano(),
		digest: sha256.Sum256(second), owner: owner,
		nlink:    fileid.FileNlinkOfOpened(file, finalInfo),
		identity: fileid.FileIdentityOfOpened(file, finalInfo),
	}, nil
}
