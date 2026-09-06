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
		if err != nil || fingerprint.identity != expected.identity || fingerprint.nlink != expected.nlink || fingerprint.discovery != expected.discovery ||
			!sameBytesFingerprint(current, expected.digest) {
			return reloadFileFingerprint{}, errConfigCommitVerification
		}
	} else {
		// An expected-absent destination is part of the CAS precondition. A
		// successful open here means another writer created the destination and
		// must never be overwritten by this transaction.
		if err == nil || !os.IsNotExist(err) || fingerprint.discovery != expected.discovery ||
			fingerprint.parentDiscovery != expected.parentDiscovery {
			return reloadFileFingerprint{}, errConfigCommitVerification
		}
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
		if err != nil || fingerprint.identity != expected.identity || fingerprint.nlink != expected.nlink || fingerprint.discovery != expected.discovery ||
			!sameBytesFingerprint(current, expected.digest) {
			return reloadFileFingerprint{}, errConfigCommitVerification
		}
	} else {
		if err == nil || !os.IsNotExist(err) || fingerprint.discovery != expected.discovery ||
			fingerprint.parentDiscovery != expected.parentDiscovery {
			return reloadFileFingerprint{}, errConfigCommitVerification
		}
	}
	if expected.exists && (expected.nlink == 0 || expected.nlink > 1) {
		return reloadFileFingerprint{}, ErrConfigHardLink
	}
	if err := verifySelectedCommitPath(selectedPath, commitPath); err != nil {
		return reloadFileFingerprint{}, err
	}
	tmp, err := stageConfigFile(commitPath, data, perm)
	if err != nil {
		return reloadFileFingerprint{}, err
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmp)
		}
	}()
	runConfigBeforeCommitRenameHook()
	renameErr := renameConfigTemp(tmp, commitPath, expected.exists)
	if renameErr != nil {
		// MoveFileEx normally reports success after publication, but an API
		// failure can be ambiguous at that boundary. A matching destination is
		// proof that this transaction may already have won; continue through the
		// post-commit oracle so callers receive a CommitOutcome and do not retry
		// a mutation that is already visible.
		targetData, _, targetReadbackErr := readStableConfigFileOwned(commitPath, expectedOwner, enforceOwner)
		if targetReadbackErr != nil || !sameBytesFingerprint(targetData, sha256.Sum256(data)) {
			return reloadFileFingerprint{}, fmt.Errorf("%w: rename config file: %v", errConfigCommitVerification, renameErr)
		}
		// The source spelling is also ambiguous after an API failure. Leave it
		// alone rather than removing an entry that could still be live.
		removeTemp = false
	} else {
		removeTemp = false
	}
	hookErr := runConfigAfterCommitRenameHookForPath(commitPath)
	parentSyncErr := syncConfigParent(filepath.Dir(commitPath))
	committed, committedFingerprint, selectedReadbackErr := readStableConfigFileOwned(selectedPath, expectedOwner, enforceOwner)
	targetData, _, targetReadbackErr := readStableConfigFileOwned(commitPath, expectedOwner, enforceOwner)
	selectedMatches := selectedReadbackErr == nil && sameBytesFingerprint(committed, sha256.Sum256(data)) &&
		committedFingerprint.parentDiscovery == expected.parentDiscovery
	targetMatches := targetReadbackErr == nil && sameBytesFingerprint(targetData, sha256.Sum256(data))
	if !selectedMatches || !targetMatches {
		causes := []error{errConfigCommitUncertain, errConfigCommitCommitted}
		if selectedReadbackErr != nil {
			causes = append(causes, selectedReadbackErr)
		}
		if targetReadbackErr != nil {
			causes = append(causes, targetReadbackErr)
		}
		if renameErr != nil {
			causes = append(causes, renameErr)
		}
		if parentSyncErr != nil {
			causes = append(causes, errAtomicWriteCommitted, errConfigCommitDurabilityUncertain, parentSyncErr)
		}
		if hookErr != nil {
			causes = append(causes, hookErr)
		}
		return reloadFileFingerprint{}, newCommitOutcome(commitPath, true, false, causes...)
	}
	if renameErr != nil || hookErr != nil || parentSyncErr != nil {
		return committedFingerprint, newCommitOutcome(commitPath, true, true, commitPostCommitCauses(renameErr, hookErr, parentSyncErr)...)
	}
	return committedFingerprint, nil
}
