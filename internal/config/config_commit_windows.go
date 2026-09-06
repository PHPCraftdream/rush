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
	stagedIdentity, err := configFileIdentityAtPath(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return reloadFileFingerprint{}, fmt.Errorf("%w: identify staged config: %v", errConfigCommitVerification, err)
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = removeConfigTempIfIdentity(tmp, stagedIdentity)
		}
	}()
	runConfigBeforeCommitRenameHook()
	renameErr := renameConfigTemp(tmp, commitPath, expected.exists)
	if renameErr != nil {
		// MoveFileEx can report an error after publishing when WRITE_THROUGH
		// cannot establish durability. A matching destination alone is not
		// enough: a pre-publication access failure can leave an old,
		// byte-identical destination in place.
		sourceIdentity, sourceErr := configFileIdentityAtPath(tmp)
		_, targetFingerprint, targetErr := readStableConfigFileOwned(commitPath, expectedOwner, enforceOwner)
		targetIsStaged := targetErr == nil && targetFingerprint.identity == stagedIdentity
		sourceIsStaged := sourceErr == nil && sourceIdentity == stagedIdentity
		if sourceIsStaged && !targetIsStaged {
			causes := []error{renameErr}
			if targetErr != nil && !os.IsNotExist(targetErr) {
				causes = append(causes, targetErr)
			} else if targetErr == nil && ((!expected.exists && targetFingerprint.exists) ||
				(expected.exists && targetFingerprint.identity != expected.identity)) {
				causes = append(causes, errConfigCommitVerification)
			}
			// The staged source is still the same inode, so publication has
			// not happened. Keep removeTemp true and let the identity-checked
			// defer remove only this transaction's temporary spelling.
			return reloadFileFingerprint{}, newCommitOutcome(commitPath, false, false, causes...)
		}
		if !targetIsStaged {
			// Neither side proves the result. Preserve an explicit committed
			// uncertainty so callers do not retry a mutation that may have
			// crossed the publication boundary.
			causes := []error{errConfigCommitUncertain, errConfigCommitCommitted, renameErr}
			if sourceErr != nil && !os.IsNotExist(sourceErr) {
				causes = append(causes, sourceErr)
			}
			if targetErr != nil {
				causes = append(causes, targetErr)
			}
			return reloadFileFingerprint{}, newCommitOutcome(commitPath, true, false, causes...)
		}
		// If the destination has the staged handle identity, publication is
		// proven even when MoveFileEx returned an error. The source can still
		// be present as an alias; identity-checked cleanup handles that case.
		removeTemp = sourceErr != nil && os.IsNotExist(sourceErr)
	}
	if renameErr == nil {
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
		if renameErr != nil {
			causes = append(causes, errAtomicWriteCommitted, errConfigCommitDurabilityUncertain)
		}
		return reloadFileFingerprint{}, newCommitOutcome(commitPath, true, false, causes...)
	}
	if renameErr != nil || hookErr != nil || parentSyncErr != nil {
		return committedFingerprint, newCommitOutcome(commitPath, true, true, windowsCommitPostCommitCauses(renameErr, hookErr, parentSyncErr)...)
	}
	return committedFingerprint, nil
}

func windowsCommitPostCommitCauses(renameErr, hookErr, parentSyncErr error) []error {
	causes := commitPostCommitCauses(renameErr, hookErr, parentSyncErr)
	if renameErr != nil {
		causes = append([]error{errAtomicWriteCommitted, errConfigCommitDurabilityUncertain}, causes...)
	}
	return causes
}
