//go:build !windows

package config

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// publishConfigTempNoReplaceAt links the fully-written temporary inode into
// the destination name and then removes the temporary name. linkat is
// atomic with respect to an existing destination: it fails with EEXIST and
// never replaces that entry. The returned bool records whether publication
// happened before a cleanup error.
func publishConfigTempNoReplaceAt(oldDirFD int, oldName string, newDirFD int, newName string) (bool, error) {
	tempIdentity, tempNlink, err := configTempIdentityAt(oldDirFD, oldName)
	if err != nil {
		return false, err
	}
	if tempNlink != 1 {
		return false, ErrConfigHardLink
	}
	if err := unix.Linkat(oldDirFD, oldName, newDirFD, newName, 0); err != nil {
		return false, err
	}
	linkedIdentity, linkedNlink, err := configTempIdentityAt(oldDirFD, oldName)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return true, err
	}
	if linkedIdentity != tempIdentity {
		return true, fmt.Errorf("%w: temporary config alias changed", errConfigCommitVerification)
	}
	if linkedNlink > 2 {
		return true, ErrConfigHardLink
	}
	if linkedNlink != 2 {
		return true, fmt.Errorf("%w: temporary config alias link count changed", errConfigCommitVerification)
	}
	if err := unlinkConfigTempAt(oldDirFD, oldName); err != nil {
		return true, err
	}
	return true, nil
}

func configTempIdentityAt(dirFD int, name string) (configFileIdentity, uint64, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return configFileIdentity{}, 0, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return configFileIdentity{}, 0, err
	}
	if !info.Mode().IsRegular() {
		return configFileIdentity{}, 0, fmt.Errorf("%w: temporary config is not a regular file", errConfigCommitVerification)
	}
	identity := configFileIdentityOf(info)
	if !identity.valid {
		return configFileIdentity{}, 0, fmt.Errorf("%w: temporary config identity unavailable", errConfigCommitVerification)
	}
	return identity, configFileNlinkOfOpened(file, info), nil
}

func cleanupConfigTempAlias(dirFD int, name string, expected configFileIdentity) error {
	identity, _, err := configTempIdentityAt(dirFD, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if identity != expected {
		return fmt.Errorf("%w: temporary config alias changed", errConfigCommitVerification)
	}
	return unlinkConfigTempAt(dirFD, name)
}

func recoverConfigTempAlias(parent *os.File, base string, expected reloadFileFingerprint, enforceOwner bool, expectedOwner int) (reloadFileFingerprint, error) {
	parentFD := int(parent.Fd())
	destinationData, destination, err := readConfigEntryAt(parentFD, base, expectedOwner, enforceOwner)
	if err != nil {
		return reloadFileFingerprint{}, err
	}
	if destination.identity != expected.identity || destination.owner != expected.owner || destination.size != expected.size ||
		destination.modTime != expected.modTime || destination.digest != expected.digest || destination.nlink != expected.nlink ||
		!sameBytesFingerprint(destinationData, expected.digest) {
		return reloadFileFingerprint{}, ErrConfigHardLink
	}

	entries, err := parent.ReadDir(-1)
	if err != nil {
		return reloadFileFingerprint{}, fmt.Errorf("scan config temporary aliases: %w", err)
	}
	var ownAlias string
	for _, entry := range entries {
		name := entry.Name()
		if !isConfigTempAliasName(base, name) {
			continue
		}
		info, infoErr := configEntryInfoAt(parentFD, name)
		if infoErr != nil || !configTempAliasPolicyAcceptable(info, expectedOwner, enforceOwner) {
			// A same-pattern entry that cannot be proven safe is foreign or
			// ambiguous. Leave it untouched and fail closed.
			return reloadFileFingerprint{}, ErrConfigHardLink
		}
		identity := configFileIdentityOf(info)
		if identity != destination.identity {
			// Never remove a same-pattern inode that is not the destination
			// inode recovered from the pinned directory.
			return reloadFileFingerprint{}, ErrConfigHardLink
		}
		if configFileNlinkOfInfo(info) != destination.nlink || ownAlias != "" {
			return reloadFileFingerprint{}, ErrConfigHardLink
		}
		ownAlias = name
	}
	if ownAlias == "" {
		return reloadFileFingerprint{}, ErrConfigHardLink
	}
	if err := cleanupConfigTempAliasOwned(parentFD, ownAlias, destination.identity, expectedOwner, enforceOwner); err != nil {
		return reloadFileFingerprint{}, fmt.Errorf("remove recovered config temporary alias: %w", err)
	}
	if err := syncConfigParentFile(parent); err != nil {
		return reloadFileFingerprint{}, err
	}

	cleanedData, cleaned, cleanedErr := readConfigEntryAt(parentFD, base, expectedOwner, enforceOwner)
	if cleanedErr != nil || cleanedData == nil || cleaned.nlink != expected.nlink-1 || cleaned.identity != expected.identity ||
		cleaned.owner != expected.owner || cleaned.size != expected.size || cleaned.modTime != expected.modTime ||
		cleaned.digest != expected.digest {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	if cleaned.nlink != 1 {
		return reloadFileFingerprint{}, ErrConfigHardLink
	}
	cleaned.discovery = expected.discovery
	cleaned.parentDiscovery = expected.parentDiscovery
	return cleaned, nil
}

func readConfigEntryAt(dirFD int, name string, expectedOwner int, enforceOwner bool) ([]byte, reloadFileFingerprint, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, reloadFileFingerprint{}, fmt.Errorf("%w: config entry is not a regular file", errConfigCommitVerification)
	}
	owner, ownerKnown := configFileOwner(info)
	if enforceOwner && (!ownerKnown || owner != expectedOwner) {
		return nil, reloadFileFingerprint{}, errConfigOwnerMismatch
	}
	first, err := readOpenedConfigBytes(file)
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	second, err := readOpenedConfigBytes(file)
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	if !bytes.Equal(first, second) {
		return nil, reloadFileFingerprint{}, errStableReadUnstable
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	if !sameConfigFileIdentity(info, finalInfo) {
		return nil, reloadFileFingerprint{}, errStableReadUnstable
	}
	identity := configFileIdentityOf(finalInfo)
	return second, reloadFileFingerprint{
		exists: true, size: int64(len(second)), modTime: finalInfo.ModTime().UnixNano(),
		digest: sha256.Sum256(second), owner: owner, nlink: configFileNlinkOfInfo(finalInfo),
		identity: identity,
	}, nil
}

func configEntryInfoAt(dirFD int, name string) (os.FileInfo, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	return info, nil
}

func configFileNlinkOfInfo(info os.FileInfo) uint64 {
	return configFileNlinkOfOpened(nil, info)
}

func cleanupConfigTempAliasOwned(dirFD int, name string, expected configFileIdentity, expectedOwner int, enforceOwner bool) error {
	info, err := configEntryInfoAt(dirFD, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !configTempAliasPolicyAcceptable(info, expectedOwner, enforceOwner) || configFileIdentityOf(info) != expected {
		return fmt.Errorf("%w: temporary config alias changed", errConfigCommitVerification)
	}
	return unlinkConfigTempAt(dirFD, name)
}

func configTempAliasPolicyAcceptable(info os.FileInfo, expectedOwner int, enforceOwner bool) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	owner, ownerKnown := configFileOwner(info)
	return !enforceOwner || ownerKnown && owner == expectedOwner
}

func isConfigTempAliasName(base, name string) bool {
	prefix := "." + base + "."
	const suffix = ".tmp"
	if len(name) != len(prefix)+32+len(suffix) || name[:len(prefix)] != prefix || name[len(name)-len(suffix):] != suffix {
		return false
	}
	for _, char := range name[len(prefix) : len(name)-len(suffix)] {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func unlinkConfigTempAt(dirFD int, name string) error {
	configTestHooks.Lock()
	hook := configTestHooks.unlinkTemp
	configTestHooks.Unlock()
	if hook != nil {
		return hook(dirFD, name)
	}
	return unix.Unlinkat(dirFD, name, 0)
}
