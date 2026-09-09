//go:build !windows && !darwin

package config

import (
	"errors"
	"io"
	"os"
)

func configDirectoryCaseSensitivityByEntries(dir string) configCaseSensitivity {
	readSnapshot := func() (configCaseDirectorySnapshot, error) {
		return configReadCaseDirectorySnapshot(dir)
	}
	probe := func(path string) (configFileIdentity, error) {
		info, probeErr := os.Lstat(path)
		if probeErr != nil {
			return configFileIdentity{}, probeErr
		}
		identity := configFileIdentityOf(info)
		if !identity.valid {
			return configFileIdentity{}, os.ErrInvalid
		}
		return identity, nil
	}
	return configCaseSensitivityFromDirectorySnapshots(dir, readSnapshot, probe)
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
	identity := configFileIdentityOf(info)
	if !identity.valid {
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
