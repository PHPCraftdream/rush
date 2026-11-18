//go:build windows

package cfgcommit

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fswap"
	"golang.org/x/sys/windows"
)

// commitConfigFile keeps the staged file open through publication. Windows
// file identity is a handle property, so resolving the staged pathname after
// closing it would reintroduce a reparse-point substitution race.
func commitConfigFile(selectedPath, commitPath string, data []byte, perm os.FileMode, expected docread.Fingerprint, expectedOwner int, enforceOwner bool, migrationHooks ...func(string) error) (docread.Fingerprint, error) {
	if expected.Exists && (!expected.Identity.Valid || expected.Nlink == 0 || expected.Nlink > 1) {
		if expected.Nlink > 1 {
			return docread.Fingerprint{}, fileid.ErrConfigHardLink
		}
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	}
	if err := verifyWindowsCommitInput(selectedPath, expected, expectedOwner, enforceOwner); err != nil {
		return docread.Fingerprint{}, err
	}
	if err := verifySelectedCommitPath(selectedPath, commitPath); err != nil {
		return docread.Fingerprint{}, err
	}
	publicationPath, err := fileid.PhysicalWindowsConfigPath(commitPath)
	if err != nil {
		return docread.Fingerprint{}, fmt.Errorf("%w: resolve config destination: %v", fileid.ErrConfigCommitVerification(), err)
	}

	parent, err := fileid.OpenWindowsConfigParent(publicationPath)
	if err != nil {
		return docread.Fingerprint{}, fmt.Errorf("%w: %v", fileid.ErrConfigCommitVerification(), err)
	}
	defer parent.Close()
	if err := verifyWindowsCommitParent(parent, publicationPath, expected); err != nil {
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	}
	if expected.ParentDiscovery != ([32]byte{}) && docread.ConfigDiscoveryFingerprint(filepath.Dir(selectedPath)) != expected.ParentDiscovery {
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	}

	runConfigBeforeCommitCheckHook()
	base := filepath.Base(publicationPath)
	if len(migrationHooks) > 0 && migrationHooks[0] != nil {
		if err := migrationHooks[0]("before-staging"); err != nil {
			return docread.Fingerprint{}, err
		}
	}
	if err := verifyWindowsCommitParent(parent, publicationPath, expected); err != nil {
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	}
	if err := verifyWindowsCommitDestinationHandle(parent, base, expected, expectedOwner, enforceOwner, selectedPath); err != nil {
		return docread.Fingerprint{}, err
	}
	staged, err := fileid.StageConfigFileHandleAt(parent, publicationPath, data, perm, migrationHooks...)
	if err != nil {
		return docread.Fingerprint{}, err
	}
	removeTemp := true
	stagedClosed := false
	defer func() {
		if removeTemp && !stagedClosed {
			// The handle is DELETE-capable and remains open until this cleanup
			// completes. Never check a pathname, close, and then delete it.
			_ = fileid.DeleteWindowsConfigHandle(staged.File)
		}
		if !stagedClosed {
			_ = staged.File.Close()
		}
	}()

	if err := verifyWindowsCommitParent(parent, publicationPath, expected); err != nil {
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	}
	if err := verifyWindowsCommitDestinationHandle(parent, base, expected, expectedOwner, enforceOwner, selectedPath); err != nil {
		return docread.Fingerprint{}, err
	}
	runConfigBeforeCommitRenameHook()
	if err := verifyWindowsCommitParent(parent, publicationPath, expected); err != nil {
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	}
	if err := verifyWindowsCommitDestinationHandle(parent, base, expected, expectedOwner, enforceOwner, selectedPath); err != nil {
		return docread.Fingerprint{}, err
	}
	if len(migrationHooks) > 0 && migrationHooks[0] != nil {
		if err := migrationHooks[0]("before-rename"); err != nil {
			return docread.Fingerprint{}, err
		}
	}
	renameErr := fswap.RenameConfigTempHandle(staged.File, staged.Path, parent, base, expected.Exists, expected, expectedOwner, enforceOwner, selectedPath)
	if renameErr != nil {
		return docread.Fingerprint{}, classifyWindowsRenameFailure(publicationPath, parent, data, expected, expectedOwner, enforceOwner, staged, renameErr, &removeTemp, &stagedClosed)
	}
	removeTemp = false
	if len(migrationHooks) > 0 && migrationHooks[0] != nil {
		if err := migrationHooks[0]("after-rename"); err != nil {
			return docread.Fingerprint{}, newCommitOutcome(commitPath, true, false, errAtomicWriteCommitted, err)
		}
	}
	flushErr := fileid.FlushWindowsConfigHandle(staged.File)
	stagedData, stagedFingerprint, stagedReadErr := docread.ReadStableConfigHandle(staged.File, expectedOwner, enforceOwner)
	_ = staged.File.Close()
	stagedClosed = true

	hookErr := runConfigAfterCommitRenameHookForPath(commitPath)
	parentSyncErr := syncConfigParentFile(parent)
	if flushErr != nil {
		parentSyncErr = errors.Join(parentSyncErr, flushErr)
	}
	committed, committedFingerprint, selectedReadbackErr := docread.ReadStableConfigFileOwned(selectedPath, expectedOwner, enforceOwner)
	targetData, _, targetReadbackErr := docread.ReadStableConfigFileOwned(publicationPath, expectedOwner, enforceOwner)
	selectedMatches := selectedReadbackErr == nil && docread.SameBytesFingerprint(committed, sha256.Sum256(data)) &&
		committedFingerprint.ParentDiscovery == expected.ParentDiscovery
	publicationProven := stagedReadErr == nil && stagedFingerprint.Identity == staged.Identity &&
		docread.SameBytesFingerprint(stagedData, sha256.Sum256(data))
	targetMatches := targetReadbackErr == nil && docread.SameBytesFingerprint(targetData, sha256.Sum256(data))
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
		return docread.Fingerprint{}, newCommitOutcome(commitPath, true, false, causes...)
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
		return docread.Fingerprint{}, newCommitOutcome(commitPath, true, false, causes...)
	}
	if hookErr != nil || parentSyncErr != nil {
		return committedFingerprint, newCommitOutcome(commitPath, true, true, commitPostCommitCauses(nil, hookErr, parentSyncErr)...)
	}
	return committedFingerprint, nil
}

func verifyWindowsCommitInput(path string, expected docread.Fingerprint, expectedOwner int, enforceOwner bool) error {
	current, fingerprint, err := docread.ReadStableConfigFileOwned(path, expectedOwner, enforceOwner)
	if expected.Exists {
		if err != nil || fingerprint.Identity != expected.Identity || fingerprint.Nlink != expected.Nlink || fingerprint.Discovery != expected.Discovery ||
			fingerprint.ParentIdentity != expected.ParentIdentity || !docread.SameBytesFingerprint(current, expected.Digest) {
			return fileid.ErrConfigCommitVerification()
		}
		return nil
	}
	if err == nil || !os.IsNotExist(err) || fingerprint.Discovery != expected.Discovery || fingerprint.ParentDiscovery != expected.ParentDiscovery ||
		(expected.ParentIdentity.Valid && fingerprint.ParentIdentity != expected.ParentIdentity) {
		return fileid.ErrConfigCommitVerification()
	}
	return nil
}

func verifyWindowsCommitParent(parent *os.File, publicationPath string, expected docread.Fingerprint) error {
	pinned := fileid.FileIdentityOfOpened(parent, nil)
	if !pinned.Valid || (expected.ParentIdentity.Valid && pinned != expected.ParentIdentity) {
		return fileid.ErrConfigCommitVerification()
	}
	current, err := fileid.OpenWindowsConfigParent(publicationPath)
	if err != nil {
		return fileid.ErrConfigCommitVerification()
	}
	defer current.Close()
	currentIdentity := fileid.FileIdentityOfOpened(current, nil)
	if !currentIdentity.Valid || currentIdentity != pinned {
		return fileid.ErrConfigCommitVerification()
	}
	return nil
}

func verifyWindowsCommitDestinationHandle(parent *os.File, name string, expected docread.Fingerprint, expectedOwner int, enforceOwner bool, discoveryPath string) error {
	file, err := fileid.OpenWindowsConfigEntryAt(parent, name)
	if !expected.Exists {
		if fileid.IsWindowsEntryNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: open destination: %v", fileid.ErrConfigCommitVerification(), err)
		}
		return fileid.ErrConfigCommitVerification()
	}
	if err != nil {
		return fmt.Errorf("%w: open destination: %v", fileid.ErrConfigCommitVerification(), err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fileid.ErrConfigCommitVerification()
	}
	handleInfo, err := fileid.WindowsFileInformation(file)
	if err != nil || !info.Mode().IsRegular() || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fileid.ErrConfigCommitVerification()
	}
	data, actual, err := docread.ReadStableConfigHandle(file, expectedOwner, enforceOwner)
	if err != nil || !expected.Exists || actual.Exists != expected.Exists || actual.Size != expected.Size ||
		actual.ModTime != expected.ModTime || actual.Digest != expected.Digest || actual.Owner != expected.Owner ||
		actual.Nlink != expected.Nlink || actual.Identity != expected.Identity ||
		!docread.SameBytesFingerprint(data, expected.Digest) {
		return fileid.ErrConfigCommitVerification()
	}
	if expected.Discovery != ([32]byte{}) && docread.ConfigDiscoveryFingerprint(discoveryPath) != expected.Discovery {
		return fileid.ErrConfigCommitVerification()
	}
	if expected.ParentDiscovery != ([32]byte{}) && docread.ConfigDiscoveryFingerprint(filepath.Dir(discoveryPath)) != expected.ParentDiscovery {
		return fileid.ErrConfigCommitVerification()
	}
	if actual.Nlink > 1 {
		return fileid.ErrConfigHardLink
	}
	return nil
}

func classifyWindowsRenameFailure(commitPath string, parent *os.File, data []byte, expected docread.Fingerprint, expectedOwner int, enforceOwner bool, staged fileid.WindowsStagedConfigFile, renameErr error, removeTemp, stagedClosed *bool) error {
	// Resolve both directory entries through the pinned parent. The staged
	// handle remains open until classification and cleanup are complete.
	temp, tempErr := fileid.OpenWindowsConfigEntryAt(parent, staged.Name)
	target, targetErr := fileid.OpenWindowsConfigEntryAt(parent, filepath.Base(commitPath))
	if temp != nil {
		defer temp.Close()
	}
	if target != nil {
		defer target.Close()
	}
	sourcePresent := tempErr == nil && fileid.FileIdentityOfOpened(temp, nil) == staged.Identity
	targetIdentity := fileid.FileIdentity{}
	if targetErr == nil {
		targetIdentity = fileid.FileIdentityOfOpened(target, nil)
	}
	targetStaged := targetErr == nil && targetIdentity == staged.Identity
	targetExpected := expected.Exists && targetErr == nil && targetIdentity == expected.Identity
	targetAbsent := fileid.IsWindowsEntryNotFound(targetErr) || os.IsNotExist(targetErr)

	if sourcePresent && !targetStaged {
		// No publication is proven. Cleanup is performed by the opened staged
		// handle; it cannot delete a foreign file at the temporary pathname.
		causes := []error{renameErr}
		if targetErr != nil && !targetAbsent {
			causes = append(causes, targetErr)
		}
		if !expected.Exists && !targetAbsent {
			causes = append(causes, fileid.ErrConfigCommitVerification())
		}
		return newCommitOutcome(commitPath, false, false, causes...)
	}
	if !sourcePresent && targetStaged {
		// The directory entry moved, but the API reported an error. Preserve the
		// write-through uncertainty and never delete the staged handle now that
		// it denotes the published destination.
		*removeTemp = false
		reconciledData, _, readErr := docread.ReadStableConfigHandle(target, expectedOwner, enforceOwner)
		reconciled := readErr == nil && docread.SameBytesFingerprint(reconciledData, sha256.Sum256(data))
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
	if tempErr != nil && !fileid.IsWindowsEntryNotFound(tempErr) && !os.IsNotExist(tempErr) {
		causes = append(causes, tempErr)
	}
	if targetErr != nil {
		causes = append(causes, targetErr)
	}
	return newMaybeCommitOutcome(commitPath, causes...)
}
