//go:build !windows

package fswap

import (
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/config/io/cfghooks"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
	"golang.org/x/sys/unix"
)

// PublishConfigTempNoReplaceAt links the fully-written temporary inode into
// the destination name and then removes the temporary name. linkat is
// atomic with respect to an existing destination: it fails with EEXIST and
// never replaces that entry. The returned bool records whether publication
// happened before a cleanup error.
func PublishConfigTempNoReplaceAt(oldDirFD int, oldName string, newDirFD int, newName string) (bool, error) {
	tempIdentity, tempNlink, err := ConfigTempIdentityAt(oldDirFD, oldName)
	if err != nil {
		return false, err
	}
	if tempNlink != 1 {
		return false, fileid.ErrConfigHardLink
	}
	if err := unix.Linkat(oldDirFD, oldName, newDirFD, newName, 0); err != nil {
		return false, err
	}
	linkedIdentity, linkedNlink, err := ConfigTempIdentityAt(oldDirFD, oldName)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return true, err
	}
	if linkedIdentity != tempIdentity {
		return true, fmt.Errorf("%w: temporary config alias changed", fileid.ErrConfigCommitVerification())
	}
	if linkedNlink > 2 {
		return true, fileid.ErrConfigHardLink
	}
	if linkedNlink != 2 {
		return true, fmt.Errorf("%w: temporary config alias link count changed", fileid.ErrConfigCommitVerification())
	}
	if err := UnlinkConfigTempAt(oldDirFD, oldName); err != nil {
		return true, err
	}
	return true, nil
}

func ConfigTempIdentityAt(dirFD int, name string) (fileid.FileIdentity, uint64, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fileid.FileIdentity{}, 0, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fileid.FileIdentity{}, 0, err
	}
	if !info.Mode().IsRegular() {
		return fileid.FileIdentity{}, 0, fmt.Errorf("%w: temporary config is not a regular file", fileid.ErrConfigCommitVerification())
	}
	identity := fileid.FileIdentityOf(info)
	if !identity.Valid {
		return fileid.FileIdentity{}, 0, fmt.Errorf("%w: temporary config identity unavailable", fileid.ErrConfigCommitVerification())
	}
	return identity, fileid.FileNlinkOfOpened(file, info), nil
}

func CleanupConfigTempAlias(dirFD int, name string, expected fileid.FileIdentity) error {
	identity, _, err := ConfigTempIdentityAt(dirFD, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if identity != expected {
		return fmt.Errorf("%w: temporary config alias changed", fileid.ErrConfigCommitVerification())
	}
	return UnlinkConfigTempAt(dirFD, name)
}

func RecoverConfigTempAlias(parent *os.File, base string, expected docread.Fingerprint, enforceOwner bool, expectedOwner int) (docread.Fingerprint, error) {
	parentFD := int(parent.Fd())
	destinationData, destination, err := docread.ReadConfigEntryAt(parentFD, base, expectedOwner, enforceOwner)
	if err != nil {
		return docread.Fingerprint{}, err
	}
	if destination.Identity != expected.Identity || destination.Owner != expected.Owner || destination.Size != expected.Size ||
		destination.ModTime != expected.ModTime || destination.Digest != expected.Digest || destination.Nlink != expected.Nlink ||
		!docread.SameBytesFingerprint(destinationData, expected.Digest) {
		return docread.Fingerprint{}, fileid.ErrConfigHardLink
	}

	entries, err := parent.ReadDir(-1)
	if err != nil {
		return docread.Fingerprint{}, fmt.Errorf("scan config temporary aliases: %w", err)
	}
	var ownAlias string
	for _, entry := range entries {
		name := entry.Name()
		if !IsConfigTempAliasName(base, name) {
			continue
		}
		info, infoErr := ConfigEntryInfoAt(parentFD, name)
		if infoErr != nil || !ConfigTempAliasPolicyAcceptable(info, expectedOwner, enforceOwner) {
			// A same-pattern entry that cannot be proven safe is foreign or
			// ambiguous. Leave it untouched and fail closed.
			return docread.Fingerprint{}, fileid.ErrConfigHardLink
		}
		identity := fileid.FileIdentityOf(info)
		if identity != destination.Identity {
			// Never remove a same-pattern inode that is not the destination
			// inode recovered from the pinned directory.
			return docread.Fingerprint{}, fileid.ErrConfigHardLink
		}
		if fileid.FileNlinkOfInfo(info) != destination.Nlink || ownAlias != "" {
			return docread.Fingerprint{}, fileid.ErrConfigHardLink
		}
		ownAlias = name
	}
	if ownAlias == "" {
		return docread.Fingerprint{}, fileid.ErrConfigHardLink
	}
	if err := CleanupConfigTempAliasOwned(parentFD, ownAlias, destination.Identity, expectedOwner, enforceOwner); err != nil {
		return docread.Fingerprint{}, fmt.Errorf("remove recovered config temporary alias: %w", err)
	}
	if err := syncConfigParentFile(parent); err != nil {
		return docread.Fingerprint{}, err
	}

	cleanedData, cleaned, cleanedErr := docread.ReadConfigEntryAt(parentFD, base, expectedOwner, enforceOwner)
	if cleanedErr != nil || cleanedData == nil || cleaned.Nlink != expected.Nlink-1 || cleaned.Identity != expected.Identity ||
		cleaned.Owner != expected.Owner || cleaned.Size != expected.Size || cleaned.ModTime != expected.ModTime ||
		cleaned.Digest != expected.Digest {
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	}
	if cleaned.Nlink != 1 {
		return docread.Fingerprint{}, fileid.ErrConfigHardLink
	}
	cleaned.Discovery = expected.Discovery
	cleaned.ParentDiscovery = expected.ParentDiscovery
	return cleaned, nil
}

func ConfigEntryInfoAt(dirFD int, name string) (os.FileInfo, error) {
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

func CleanupConfigTempAliasOwned(dirFD int, name string, expected fileid.FileIdentity, expectedOwner int, enforceOwner bool) error {
	info, err := ConfigEntryInfoAt(dirFD, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !ConfigTempAliasPolicyAcceptable(info, expectedOwner, enforceOwner) || fileid.FileIdentityOf(info) != expected {
		return fmt.Errorf("%w: temporary config alias changed", fileid.ErrConfigCommitVerification())
	}
	return UnlinkConfigTempAt(dirFD, name)
}

func ConfigTempAliasPolicyAcceptable(info os.FileInfo, expectedOwner int, enforceOwner bool) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	owner, ownerKnown := fileid.ConfigFileOwner(info)
	return !enforceOwner || ownerKnown && owner == expectedOwner
}

func IsConfigTempAliasName(base, name string) bool {
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

func UnlinkConfigTempAt(dirFD int, name string) error {
	if hook := cfghooks.Snapshot().UnlinkTemp; hook != nil {
		return hook(dirFD, name)
	}
	return unix.Unlinkat(dirFD, name, 0)
}

// syncConfigParentFile mirrors cfgcommit's SyncConfigParentFile hook
// consumption; fswap cannot import cfgcommit (it imports fswap).
func syncConfigParentFile(file *os.File) error {
	hooks := cfghooks.Snapshot()
	if hooks.SyncParentFD != nil {
		return hooks.SyncParentFD(int(file.Fd()))
	}
	if hooks.SyncParentFile != nil {
		return hooks.SyncParentFile(file)
	}
	if hooks.SyncParent != nil {
		return hooks.SyncParent(file.Name())
	}
	return file.Sync()
}
