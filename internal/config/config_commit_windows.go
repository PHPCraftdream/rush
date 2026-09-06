//go:build windows

package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// commitConfigFile keeps the staged file open through publication. Windows
// file identity is a handle property, so resolving the staged pathname after
// closing it would reintroduce a reparse-point substitution race.
func commitConfigFile(selectedPath, commitPath string, data []byte, perm os.FileMode, expected reloadFileFingerprint, expectedOwner int, enforceOwner bool) (reloadFileFingerprint, error) {
	if expected.exists && (!expected.identity.valid || expected.nlink == 0 || expected.nlink > 1) {
		if expected.nlink > 1 {
			return reloadFileFingerprint{}, ErrConfigHardLink
		}
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	if err := verifyWindowsCommitInput(selectedPath, expected, expectedOwner, enforceOwner); err != nil {
		return reloadFileFingerprint{}, err
	}
	if err := verifySelectedCommitPath(selectedPath, commitPath); err != nil {
		return reloadFileFingerprint{}, err
	}
	publicationPath, err := physicalWindowsConfigPath(commitPath)
	if err != nil {
		return reloadFileFingerprint{}, fmt.Errorf("%w: resolve config destination: %v", errConfigCommitVerification, err)
	}

	parent, err := openWindowsConfigParent(publicationPath)
	if err != nil {
		return reloadFileFingerprint{}, fmt.Errorf("%w: %v", errConfigCommitVerification, err)
	}
	defer parent.Close()
	if err := verifyWindowsCommitParent(parent, publicationPath, expected); err != nil {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	if expected.parentDiscovery != ([32]byte{}) && configDiscoveryFingerprint(filepath.Dir(selectedPath)) != expected.parentDiscovery {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}

	runConfigBeforeCommitCheckHook()
	base := filepath.Base(publicationPath)
	if err := verifyWindowsCommitParent(parent, publicationPath, expected); err != nil {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	if err := verifyWindowsCommitDestinationHandle(parent, base, expected); err != nil {
		return reloadFileFingerprint{}, err
	}
	staged, err := stageConfigFileHandleAt(parent, publicationPath, data, perm)
	if err != nil {
		return reloadFileFingerprint{}, err
	}
	removeTemp := true
	stagedClosed := false
	defer func() {
		if removeTemp && !stagedClosed {
			// The handle is DELETE-capable and remains open until this cleanup
			// completes. Never check a pathname, close, and then delete it.
			_ = deleteWindowsConfigHandle(staged.file)
		}
		if !stagedClosed {
			_ = staged.file.Close()
		}
	}()

	if err := verifyWindowsCommitParent(parent, publicationPath, expected); err != nil {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	if err := verifyWindowsCommitDestinationHandle(parent, base, expected); err != nil {
		return reloadFileFingerprint{}, err
	}
	runConfigBeforeCommitRenameHook()
	if err := verifyWindowsCommitParent(parent, publicationPath, expected); err != nil {
		return reloadFileFingerprint{}, errConfigCommitVerification
	}
	if err := verifyWindowsCommitDestinationHandle(parent, base, expected); err != nil {
		return reloadFileFingerprint{}, err
	}
	renameErr := renameConfigTempHandle(staged.file, staged.path, parent, base, expected.exists)
	if renameErr != nil {
		return reloadFileFingerprint{}, classifyWindowsRenameFailure(publicationPath, parent, data, expected, expectedOwner, enforceOwner, staged, renameErr, &removeTemp, &stagedClosed)
	}
	removeTemp = false
	flushErr := flushWindowsConfigHandle(staged.file)
	stagedData, stagedFingerprint, stagedReadErr := readStableConfigHandle(staged.file, expectedOwner, enforceOwner)
	_ = staged.file.Close()
	stagedClosed = true

	hookErr := runConfigAfterCommitRenameHookForPath(commitPath)
	parentSyncErr := syncConfigParentFile(parent)
	if flushErr != nil {
		parentSyncErr = errors.Join(parentSyncErr, flushErr)
	}
	committed, committedFingerprint, selectedReadbackErr := readStableConfigFileOwned(selectedPath, expectedOwner, enforceOwner)
	targetData, _, targetReadbackErr := readStableConfigFileOwned(publicationPath, expectedOwner, enforceOwner)
	selectedMatches := selectedReadbackErr == nil && sameBytesFingerprint(committed, sha256.Sum256(data)) &&
		committedFingerprint.parentDiscovery == expected.parentDiscovery
	publicationProven := stagedReadErr == nil && stagedFingerprint.identity == staged.identity &&
		sameBytesFingerprint(stagedData, sha256.Sum256(data))
	targetMatches := targetReadbackErr == nil && sameBytesFingerprint(targetData, sha256.Sum256(data))
	if publicationProven && !selectedMatches {
		// The pinned parent may have been renamed after it was opened. In that
		// case the handle proves publication, while the requested pathname may
		// now denote an unrelated decoy. Keep the committed outcome explicit.
		causes := []error{errConfigCommitCommitted, errConfigCommitUncertain}
		if selectedReadbackErr != nil {
			causes = append(causes, selectedReadbackErr)
		}
		if targetReadbackErr != nil {
			causes = append(causes, targetReadbackErr)
		}
		if hookErr != nil {
			causes = append(causes, hookErr)
		}
		if parentSyncErr != nil {
			causes = append(causes, errAtomicWriteCommitted, errConfigCommitDurabilityUncertain, parentSyncErr)
		}
		return reloadFileFingerprint{}, newCommitOutcome(commitPath, true, false, causes...)
	}
	if !selectedMatches || !targetMatches {
		causes := []error{errConfigCommitUncertain, errConfigCommitCommitted}
		if selectedReadbackErr != nil {
			causes = append(causes, selectedReadbackErr)
		}
		if targetReadbackErr != nil {
			causes = append(causes, targetReadbackErr)
		}
		if hookErr != nil {
			causes = append(causes, hookErr)
		}
		if parentSyncErr != nil {
			causes = append(causes, errAtomicWriteCommitted, errConfigCommitDurabilityUncertain, parentSyncErr)
		}
		return reloadFileFingerprint{}, newCommitOutcome(commitPath, true, false, causes...)
	}
	if hookErr != nil || parentSyncErr != nil {
		return committedFingerprint, newCommitOutcome(commitPath, true, true, commitPostCommitCauses(nil, hookErr, parentSyncErr)...)
	}
	return committedFingerprint, nil
}

func verifyWindowsCommitInput(path string, expected reloadFileFingerprint, expectedOwner int, enforceOwner bool) error {
	current, fingerprint, err := readStableConfigFileOwned(path, expectedOwner, enforceOwner)
	if expected.exists {
		if err != nil || fingerprint.identity != expected.identity || fingerprint.nlink != expected.nlink || fingerprint.discovery != expected.discovery ||
			fingerprint.parentIdentity != expected.parentIdentity || !sameBytesFingerprint(current, expected.digest) {
			return errConfigCommitVerification
		}
		return nil
	}
	if err == nil || !os.IsNotExist(err) || fingerprint.discovery != expected.discovery || fingerprint.parentDiscovery != expected.parentDiscovery ||
		(expected.parentIdentity.valid && fingerprint.parentIdentity != expected.parentIdentity) {
		return errConfigCommitVerification
	}
	return nil
}

func verifyWindowsCommitParent(parent *os.File, publicationPath string, expected reloadFileFingerprint) error {
	pinned := configFileIdentityOfOpened(parent, nil)
	if !pinned.valid || (expected.parentIdentity.valid && pinned != expected.parentIdentity) {
		return errConfigCommitVerification
	}
	current, err := openWindowsConfigParent(publicationPath)
	if err != nil {
		return errConfigCommitVerification
	}
	defer current.Close()
	currentIdentity := configFileIdentityOfOpened(current, nil)
	if !currentIdentity.valid || currentIdentity != pinned {
		return errConfigCommitVerification
	}
	return nil
}

func physicalWindowsConfigPath(path string) (string, error) {
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(resolved), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(clean))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(clean)), nil
}

func verifyWindowsCommitDestinationHandle(parent *os.File, name string, expected reloadFileFingerprint) error {
	file, err := openWindowsConfigEntryAt(parent, name)
	if !expected.exists {
		if isWindowsEntryNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: open destination: %v", errConfigCommitVerification, err)
		}
		return errConfigCommitVerification
	}
	if err != nil {
		return fmt.Errorf("%w: open destination: %v", errConfigCommitVerification, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return errConfigCommitVerification
	}
	handleInfo, err := windowsFileInformation(file)
	if err != nil || !info.Mode().IsRegular() || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errConfigCommitVerification
	}
	identity := configFileIdentityOfOpened(file, info)
	if identity != expected.identity {
		return errConfigCommitVerification
	}
	nlink := configFileNlinkOfOpened(file, info)
	if nlink != expected.nlink || nlink > 1 {
		return ErrConfigHardLink
	}
	return nil
}

func classifyWindowsRenameFailure(commitPath string, parent *os.File, data []byte, expected reloadFileFingerprint, expectedOwner int, enforceOwner bool, staged windowsStagedConfigFile, renameErr error, removeTemp, stagedClosed *bool) error {
	// Resolve both directory entries through the pinned parent. The staged
	// handle remains open until classification and cleanup are complete.
	temp, tempErr := openWindowsConfigEntryAt(parent, staged.name)
	target, targetErr := openWindowsConfigEntryAt(parent, filepath.Base(commitPath))
	if temp != nil {
		defer temp.Close()
	}
	if target != nil {
		defer target.Close()
	}
	sourcePresent := tempErr == nil && configFileIdentityOfOpened(temp, nil) == staged.identity
	targetIdentity := configFileIdentity{}
	if targetErr == nil {
		targetIdentity = configFileIdentityOfOpened(target, nil)
	}
	targetStaged := targetErr == nil && targetIdentity == staged.identity
	targetExpected := expected.exists && targetErr == nil && targetIdentity == expected.identity
	targetAbsent := isWindowsEntryNotFound(targetErr) || os.IsNotExist(targetErr)

	if sourcePresent && !targetStaged {
		// No publication is proven. Cleanup is performed by the opened staged
		// handle; it cannot delete a foreign file at the temporary pathname.
		causes := []error{renameErr}
		if targetErr != nil && !targetAbsent {
			causes = append(causes, targetErr)
		}
		if !expected.exists && !targetAbsent {
			causes = append(causes, errConfigCommitVerification)
		}
		return newCommitOutcome(commitPath, false, false, causes...)
	}
	if !sourcePresent && targetStaged {
		// The directory entry moved, but the API reported an error. Preserve the
		// write-through uncertainty and never delete the staged handle now that
		// it denotes the published destination.
		*removeTemp = false
		reconciledData, _, readErr := readStableConfigHandle(target, expectedOwner, enforceOwner)
		reconciled := readErr == nil && sameBytesFingerprint(reconciledData, sha256.Sum256(data))
		causes := []error{errConfigCommitCommitted, errAtomicWriteCommitted, errConfigCommitDurabilityUncertain, renameErr}
		if readErr != nil {
			causes = append(causes, readErr)
		}
		return newCommitOutcome(commitPath, true, reconciled, causes...)
	}
	if sourcePresent && targetExpected {
		return newCommitOutcome(commitPath, false, false, renameErr)
	}

	// Neither side gives a complete identity proof. Committed must stay false:
	// callers can distinguish this byte-free state from a proven commit and
	// must not retry without reconciling it themselves.
	causes := []error{errConfigCommitUncertain, renameErr}
	if tempErr != nil && !isWindowsEntryNotFound(tempErr) && !os.IsNotExist(tempErr) {
		causes = append(causes, tempErr)
	}
	if targetErr != nil {
		causes = append(causes, targetErr)
	}
	return newMaybeCommitOutcome(commitPath, causes...)
}
