//go:build !windows

package cfgcommit

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
	"github.com/PHPCraftdream/rush/internal/config/io/fswap"
	"golang.org/x/sys/unix"
)

// commitConfigFile verifies the selected spelling, pins commitPath as the
// physical transaction target, and commits through a descriptor for its
// parent. An external symlink retarget after that boundary is not serialized
// with rename; readback reports it as an uncertain outcome.
func commitConfigFile(selectedPath, commitPath string, data []byte, perm os.FileMode, expected docread.Fingerprint, expectedOwner int, enforceOwner bool, migrationHooks ...func(string) error) (docread.Fingerprint, error) {
	if selectedPath == "" || commitPath == "" {
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	}
	current, currentFingerprint, err := docread.ReadStableConfigFileOwned(selectedPath, expectedOwner, enforceOwner)
	if err != nil {
		if !expected.Exists && os.IsNotExist(err) && currentFingerprint.Discovery == expected.Discovery && currentFingerprint.ParentDiscovery == expected.ParentDiscovery {
			// The selected target is still absent and its discovery chain is
			// unchanged. The parent descriptor check below completes validation.
		} else {
			return docread.Fingerprint{}, fmt.Errorf("%w: %v", fileid.ErrConfigCommitVerification(), err)
		}
	} else if !expected.Exists || currentFingerprint.Identity != expected.Identity ||
		currentFingerprint.Owner != expected.Owner || currentFingerprint.Nlink != expected.Nlink || currentFingerprint.Discovery != expected.Discovery ||
		!docread.SameBytesFingerprint(current, expected.Digest) {
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	}

	parentPath := filepath.Dir(commitPath)
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return docread.Fingerprint{}, fmt.Errorf("%w: open parent: %v", fileid.ErrConfigCommitVerification(), err)
	}
	parent := os.NewFile(uintptr(parentFD), parentPath)
	defer parent.Close()
	parentInfo, err := parent.Stat()
	if err != nil {
		return docread.Fingerprint{}, fmt.Errorf("%w: stat parent: %v", fileid.ErrConfigCommitVerification(), err)
	}
	parentOwner, parentOwnerKnown := fileid.ConfigFileOwner(parentInfo)
	if enforceOwner && (!parentOwnerKnown || parentOwner != expectedOwner) {
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	}
	if expected.ParentDiscovery != ([32]byte{}) &&
		docread.ConfigDiscoveryFingerprint(filepath.Dir(selectedPath)) != expected.ParentDiscovery {
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	}

	base := filepath.Base(commitPath)
	if expected.Exists && expected.Nlink > 1 {
		expected, err = recoverConfigTempAlias(parent, base, expected, enforceOwner, expectedOwner)
		if err != nil {
			return docread.Fingerprint{}, err
		}
	}
	if err := verifyCommitEntry(parentFD, base, expected, enforceOwner, expectedOwner); err != nil {
		return docread.Fingerprint{}, err
	}
	if expected.Exists && (expected.Nlink == 0 || expected.Nlink > 1) {
		return docread.Fingerprint{}, fileid.ErrConfigHardLink
	}
	if len(migrationHooks) > 0 && migrationHooks[0] != nil {
		if err := migrationHooks[0]("before-staging"); err != nil {
			return docread.Fingerprint{}, err
		}
	}
	tmpFD, tmpName, err := openUniqueConfigTemp(parentFD, base, perm)
	if err != nil {
		return docread.Fingerprint{}, err
	}
	tmp := os.NewFile(uintptr(tmpFD), tmpName)
	removeTemp := true
	tmpInfo, err := tmp.Stat()
	if err != nil {
		_ = tmp.Close()
		_ = unlinkConfigTempAt(parentFD, tmpName)
		return docread.Fingerprint{}, err
	}
	tmpIdentity := fileid.FileIdentityOf(tmpInfo)
	defer func() {
		_ = tmp.Close()
		if removeTemp {
			_ = cleanupConfigTempAlias(parentFD, tmpName, tmpIdentity)
		}
	}()
	half := len(data) / 2
	if _, err := tmp.Write(data[:half]); err != nil {
		return docread.Fingerprint{}, err
	}
	if len(migrationHooks) > 0 && migrationHooks[0] != nil {
		if err := migrationHooks[0]("during-staging"); err != nil {
			removeTemp = false
			_ = tmp.Close()
			return docread.Fingerprint{}, err
		}
	}
	if _, err := tmp.Write(data[half:]); err != nil {
		return docread.Fingerprint{}, err
	}
	if err := tmp.Chmod(perm); err != nil {
		return docread.Fingerprint{}, err
	}
	if err := tmp.Sync(); err != nil {
		return docread.Fingerprint{}, err
	}
	if len(migrationHooks) > 0 && migrationHooks[0] != nil {
		if err := migrationHooks[0]("after-staging"); err != nil {
			removeTemp = false
			return docread.Fingerprint{}, err
		}
	}
	if err := tmp.Close(); err != nil {
		return docread.Fingerprint{}, err
	}
	runConfigBeforeCommitCheckHook()
	if current, fingerprint, readErr := docread.ReadStableConfigFileOwned(selectedPath, expectedOwner, enforceOwner); readErr == nil {
		if !expected.Exists || fingerprint.Identity != expected.Identity || fingerprint.Owner != expected.Owner || fingerprint.Nlink != expected.Nlink ||
			fingerprint.Discovery != expected.Discovery || !docread.SameBytesFingerprint(current, expected.Digest) {
			return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
		}
	} else if expected.Exists || !os.IsNotExist(readErr) {
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	} else if fingerprint.Discovery != expected.Discovery || fingerprint.ParentDiscovery != expected.ParentDiscovery {
		return docread.Fingerprint{}, fileid.ErrConfigCommitVerification()
	}
	if err := verifySelectedCommitPath(selectedPath, commitPath); err != nil {
		return docread.Fingerprint{}, err
	}
	if err := verifyCommitEntry(parentFD, base, expected, enforceOwner, expectedOwner); err != nil {
		return docread.Fingerprint{}, err
	}
	runConfigBeforeCommitRenameHook()
	if len(migrationHooks) > 0 && migrationHooks[0] != nil {
		if err := migrationHooks[0]("before-rename"); err != nil {
			return docread.Fingerprint{}, err
		}
	}
	published, renameErr := fswap.RenameConfigTempAt(parentFD, tmpName, parentFD, base, expected.Exists)
	if renameErr != nil && !published {
		if !expected.Exists {
			return docread.Fingerprint{}, fmt.Errorf("%w: no-replace rename: %w", fileid.ErrConfigCommitVerification(), renameErr)
		}
		return docread.Fingerprint{}, renameErr
	}
	if renameErr == nil {
		removeTemp = false
	} else if published {
		// A linkat-based no-replace publication can fail after the destination
		// link is visible, while removing the temporary spelling. Retry only
		// after proving that the name still denotes our original inode.
		if cleanupErr := cleanupConfigTempAlias(parentFD, tmpName, tmpIdentity); cleanupErr == nil {
			removeTemp = false
		} else {
			renameErr = errors.Join(renameErr, cleanupErr)
		}
		// The publication boundary has passed. Do not perform another
		// pathname mutation after the parent descriptor is synced.
		removeTemp = false
	}

	// From this point onward the logical mutation has happened. Keep the
	// result distinct from a pre-rename verification failure so callers do not
	// retry a mutation that may already be visible on disk.
	if len(migrationHooks) > 0 && migrationHooks[0] != nil {
		if err := migrationHooks[0]("after-rename"); err != nil {
			return docread.Fingerprint{}, newCommitOutcome(commitPath, true, false, errAtomicWriteCommitted, err)
		}
	}
	hookErr := error(nil)
	if err := runConfigAfterCommitRenameHookForPath(commitPath); err != nil {
		hookErr = err
	}
	parentSyncErr := syncConfigParentFile(parent)
	committed, committedFingerprint, selectedReadbackErr := docread.ReadStableConfigFileOwned(selectedPath, expectedOwner, enforceOwner)
	targetData, _, targetReadbackErr := docread.ReadStableConfigFileOwned(commitPath, expectedOwner, enforceOwner)
	selectedMatches := selectedReadbackErr == nil && docread.SameBytesFingerprint(committed, sha256.Sum256(data)) &&
		committedFingerprint.ParentDiscovery == expected.ParentDiscovery &&
		(!expected.Exists && committedFingerprint.Discovery != expected.Discovery || expected.Exists)
	targetMatches := targetReadbackErr == nil && docread.SameBytesFingerprint(targetData, sha256.Sum256(data))
	if !selectedMatches || !targetMatches ||
		(!expected.Exists && committedFingerprint.Discovery == expected.Discovery) {
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
		return docread.Fingerprint{}, newCommitOutcome(commitPath, true, false, causes...)
	}
	if renameErr != nil || hookErr != nil || parentSyncErr != nil {
		return committedFingerprint, newCommitOutcome(commitPath, true, true, commitPostCommitCauses(renameErr, hookErr, parentSyncErr)...)
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
	return -1, "", fmt.Errorf("create temporary config: %w", fileid.ErrConfigCommitVerification())
}

func verifyCommitEntry(parentFD int, base string, expected docread.Fingerprint, enforceOwner bool, expectedOwner int) error {
	fd, err := unix.Openat(parentFD, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if !expected.Exists && err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return fmt.Errorf("%w: inspect destination: %v", fileid.ErrConfigCommitVerification(), err)
	}
	if err != nil {
		return fmt.Errorf("%w: inspect destination: %v", fileid.ErrConfigCommitVerification(), err)
	}
	file := os.NewFile(uintptr(fd), base)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat destination: %v", fileid.ErrConfigCommitVerification(), err)
	}
	identity := fileid.FileIdentityOf(info)
	owner, ownerKnown := fileid.ConfigFileOwner(info)
	if !expected.Exists || !identity.Valid || identity != expected.Identity ||
		(enforceOwner && (!ownerKnown || owner != expectedOwner)) {
		return fileid.ErrConfigCommitVerification()
	}
	if expected.Nlink == 0 || expected.Nlink > 1 || fileid.FileNlinkOfOpened(file, info) != expected.Nlink {
		return fileid.ErrConfigHardLink
	}
	return nil
}
