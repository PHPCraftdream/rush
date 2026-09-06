//go:build !windows

package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// commitConfigFile verifies the selected spelling and then commits through a
// descriptor for the resolved physical parent. The final rename therefore
// cannot follow a replacement parent directory or replace a symlink alias;
// commitPath is the physical target selected by the opened-file identity.
func commitConfigFile(selectedPath, commitPath string, data []byte, perm os.FileMode, expected reloadFileFingerprint, expectedOwner int, enforceOwner bool) (reloadFileFingerprint, error) {
	if selectedPath == "" || commitPath == "" {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	current, currentFingerprint, err := readStableConfigFileOwned(selectedPath, expectedOwner, enforceOwner)
	if err != nil {
		if !expected.exists && os.IsNotExist(err) && currentFingerprint.discovery == expected.discovery && currentFingerprint.parentDiscovery == expected.parentDiscovery {
			// The selected target is still absent and its discovery chain is
			// unchanged. The parent descriptor check below completes validation.
		} else {
			return reloadFileFingerprint{}, fmt.Errorf("%w: %v", errConfigCommitVerification, err)
		}
	} else if !expected.exists || currentFingerprint.identity != expected.identity ||
		currentFingerprint.owner != expected.owner || currentFingerprint.discovery != expected.discovery ||
		!sameBytesFingerprint(current, expected.digest) {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}

	parentPath := filepath.Dir(commitPath)
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return reloadFileFingerprint{}, fmt.Errorf("%w: open parent: %v", errConfigCommitVerification, err)
	}
	parent := os.NewFile(uintptr(parentFD), parentPath)
	defer parent.Close()
	parentInfo, err := parent.Stat()
	if err != nil {
		return reloadFileFingerprint{}, fmt.Errorf("%w: stat parent: %v", errConfigCommitVerification, err)
	}
	parentOwner, parentOwnerKnown := configFileOwner(parentInfo)
	if enforceOwner && (!parentOwnerKnown || parentOwner != expectedOwner) {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	if expected.parentDiscovery != ([32]byte{}) &&
		configDiscoveryFingerprint(filepath.Dir(selectedPath)) != expected.parentDiscovery {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}

	base := filepath.Base(commitPath)
	if err := verifyCommitEntry(parentFD, base, expected, enforceOwner, expectedOwner); err != nil {
		return reloadFileFingerprint{}, err
	}
	tmpFD, tmpName, err := openUniqueConfigTemp(parentFD, base, perm)
	if err != nil {
		return reloadFileFingerprint{}, err
	}
	tmp := os.NewFile(uintptr(tmpFD), tmpName)
	removeTemp := true
	defer func() {
		_ = tmp.Close()
		if removeTemp {
			_ = unix.Unlinkat(parentFD, tmpName, 0)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return reloadFileFingerprint{}, err
	}
	if err := tmp.Chmod(perm); err != nil {
		return reloadFileFingerprint{}, err
	}
	if err := tmp.Sync(); err != nil {
		return reloadFileFingerprint{}, err
	}
	if err := tmp.Close(); err != nil {
		return reloadFileFingerprint{}, err
	}
	runConfigBeforeCommitCheckHook()
	if current, fingerprint, readErr := readStableConfigFileOwned(selectedPath, expectedOwner, enforceOwner); readErr == nil {
		if !expected.exists || fingerprint.identity != expected.identity || fingerprint.owner != expected.owner ||
			fingerprint.discovery != expected.discovery || !sameBytesFingerprint(current, expected.digest) {
			return reloadFileFingerprint{}, errConfigCommitVerification
		}
	} else if expected.exists || !os.IsNotExist(readErr) {
		return reloadFileFingerprint{}, errConfigCommitVerification
	} else if fingerprint.discovery != expected.discovery || fingerprint.parentDiscovery != expected.parentDiscovery {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	if err := verifyCommitEntry(parentFD, base, expected, enforceOwner, expectedOwner); err != nil {
		return reloadFileFingerprint{}, err
	}
	if err := unix.Renameat(parentFD, tmpName, parentFD, base); err != nil {
		return reloadFileFingerprint{}, err
	}
	removeTemp = false

	committed, committedFingerprint, err := readStableConfigFileOwned(selectedPath, expectedOwner, enforceOwner)
	if err != nil || !sameBytesFingerprint(committed, sha256.Sum256(data)) ||
		committedFingerprint.parentDiscovery != expected.parentDiscovery ||
		(!expected.exists && committedFingerprint.discovery == expected.discovery) {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	return committedFingerprint, nil
}

const configTempNameAttempts = 16

func openUniqueConfigTemp(parentFD int, base string, perm os.FileMode) (int, string, error) {
	for range configTempNameAttempts {
		suffix := make([]byte, 16)
		if _, err := rand.Read(suffix); err != nil {
			return -1, "", fmt.Errorf("create temporary config name: %w", err)
		}
		name := "." + base + "." + hex.EncodeToString(suffix) + ".tmp"
		fd, err := unix.Openat(parentFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(perm.Perm()))
		if err == nil {
			return fd, name, nil
		}
		if err != unix.EEXIST {
			return -1, "", fmt.Errorf("create temporary config: %w", err)
		}
	}
	return -1, "", fmt.Errorf("create temporary config: %w", errConfigCommitVerification)
}

func verifyCommitEntry(parentFD int, base string, expected reloadFileFingerprint, enforceOwner bool, expectedOwner int) error {
	fd, err := unix.Openat(parentFD, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if !expected.exists && err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return fmt.Errorf("%w: inspect destination: %v", errConfigCommitVerification, err)
	}
	if err != nil {
		return fmt.Errorf("%w: inspect destination: %v", errConfigCommitVerification, err)
	}
	file := os.NewFile(uintptr(fd), base)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat destination: %v", errConfigCommitVerification, err)
	}
	identity := configFileIdentityOf(info)
	owner, ownerKnown := configFileOwner(info)
	if !expected.exists || !identity.valid || identity != expected.identity ||
		(enforceOwner && (!ownerKnown || owner != expectedOwner)) {
		return errConfigCommitVerification
	}
	return nil
}
