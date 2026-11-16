//go:build !windows && !darwin

package casefold

import (
	"errors"
	"io"
	"os"

	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
)

func configDirectoryCaseSensitivityByEntries(dir string) ConfigCaseSensitivity {
	readSnapshot := func() (configCaseDirectorySnapshot, error) {
		return configReadCaseDirectorySnapshot(dir)
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

func configReadCaseDirectorySnapshot(dir string) (configCaseDirectorySnapshot, error) {
	directory, err := os.Open(dir)
	if err != nil {
		return configCaseDirectorySnapshot{}, err
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return configCaseDirectorySnapshot{}, err
	}
	if !info.IsDir() {
		return configCaseDirectorySnapshot{}, os.ErrInvalid
	}
	identity := fileid.FileIdentityOf(info)
	if !identity.Valid {
		return configCaseDirectorySnapshot{}, os.ErrInvalid
	}
	names, err := directory.Readdirnames(configCaseDirectoryMaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return configCaseDirectorySnapshot{}, err
	}
	if configCaseDirectoryBudgetExceeded(names) {
		return configCaseDirectorySnapshot{}, errConfigCaseDirectoryBudgetExceeded
	}
	snapshot := configCaseDirectorySnapshot{
		identity: identity,
		modTime:  info.ModTime().UnixNano(),
		size:     info.Size(),
		entries:  make([]configCaseDirectoryEntry, 0, len(names)),
	}
	for _, name := range names {
		snapshot.entries = append(snapshot.entries, configCaseDirectoryEntry{
			name: name,
		})
	}
	return snapshot, nil
}
