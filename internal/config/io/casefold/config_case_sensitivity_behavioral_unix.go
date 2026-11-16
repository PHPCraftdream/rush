//go:build !windows && !darwin

package casefold

import (
	"errors"
	"io"
	"os"

	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
)

func DirectoryCaseSensitivityByEntries(dir string) ConfigCaseSensitivity {
	readSnapshot := func() (CaseDirectorySnapshot, error) {
		return ReadCaseDirectorySnapshot(dir)
	}
	probe := func(path string) (fileid.FileIdentity, error) {
		info, probeErr := os.Lstat(path)
		if probeErr != nil {
			return fileid.FileIdentity{}, probeErr
		}
		identity := fileid.FileIdentityOf(info)
		if !identity.Valid {
			return fileid.FileIdentity{}, os.ErrInvalid
		}
		return identity, nil
	}
	return ConfigCaseSensitivityFromDirectorySnapshots(dir, readSnapshot, probe)
}

func ReadCaseDirectorySnapshot(dir string) (CaseDirectorySnapshot, error) {
	directory, err := os.Open(dir)
	if err != nil {
		return CaseDirectorySnapshot{}, err
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return CaseDirectorySnapshot{}, err
	}
	if !info.IsDir() {
		return CaseDirectorySnapshot{}, os.ErrInvalid
	}
	identity := fileid.FileIdentityOf(info)
	if !identity.Valid {
		return CaseDirectorySnapshot{}, os.ErrInvalid
	}
	names, err := directory.Readdirnames(CaseDirectoryMaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return CaseDirectorySnapshot{}, err
	}
	if CaseDirectoryBudgetExceeded(names) {
		return CaseDirectorySnapshot{}, errConfigCaseDirectoryBudgetExceeded
	}
	snapshot := CaseDirectorySnapshot{
		Identity: identity,
		ModTime:  info.ModTime().UnixNano(),
		Size:     info.Size(),
		Entries:  make([]CaseDirectoryEntry, 0, len(names)),
	}
	for _, name := range names {
		snapshot.Entries = append(snapshot.Entries, CaseDirectoryEntry{
			Name: name,
		})
	}
	return snapshot, nil
}
