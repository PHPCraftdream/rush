//go:build windows

package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// Windows contract: file identity is available from the opened handle, but
// descriptor-relative no-follow rename is not exposed by the portable API.
// The transaction validates identity, owner, bytes, and discovery chain
// immediately before and after the atomic rename; the inter-process sidecar
// lock prevents cooperating Rush processes from racing. An identity-sensitive
// commit is rejected when the handle cannot provide an identity.
func commitConfigFile(selectedPath, commitPath string, data []byte, perm os.FileMode, expected reloadFileFingerprint, expectedOwner int, enforceOwner bool) (reloadFileFingerprint, error) {
	if expected.exists && !expected.identity.valid {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	current, fingerprint, err := readStableConfigFileOwned(selectedPath, expectedOwner, enforceOwner)
	if expected.exists {
		if err != nil || fingerprint.identity != expected.identity || fingerprint.discovery != expected.discovery ||
			!sameBytesFingerprint(current, expected.digest) {
			return reloadFileFingerprint{}, errConfigCommitVerification
		}
	} else if err != nil && (!os.IsNotExist(err) || fingerprint.discovery != expected.discovery) {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	parent := filepath.Dir(commitPath)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return reloadFileFingerprint{}, fmt.Errorf("%w: open parent: %v", errConfigCommitVerification, err)
	}
	parentOwner, ownerKnown := configFileOwner(parentInfo)
	if enforceOwner && (!ownerKnown || parentOwner != expectedOwner) {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	if expected.parentDiscovery != ([32]byte{}) && configDiscoveryFingerprint(filepath.Dir(selectedPath)) != expected.parentDiscovery {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	runConfigBeforeCommitCheckHook()
	current, fingerprint, err = readStableConfigFileOwned(selectedPath, expectedOwner, enforceOwner)
	if expected.exists {
		if err != nil || fingerprint.identity != expected.identity || fingerprint.discovery != expected.discovery ||
			!sameBytesFingerprint(current, expected.digest) {
			return reloadFileFingerprint{}, errConfigCommitVerification
		}
	} else if err != nil && (!os.IsNotExist(err) || fingerprint.discovery != expected.discovery) {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	if err := atomicWriteFile(commitPath, data, perm); err != nil {
		return reloadFileFingerprint{}, err
	}
	committed, committedFingerprint, err := readStableConfigFileOwned(selectedPath, expectedOwner, enforceOwner)
	if err != nil || !sameBytesFingerprint(committed, sha256.Sum256(data)) ||
		committedFingerprint.parentDiscovery != expected.parentDiscovery {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	return committedFingerprint, nil
}
