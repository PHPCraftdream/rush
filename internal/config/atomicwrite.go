package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var errConfigCommitVerification = errors.New("config destination could not be verified immediately before commit")

// errConfigCommitCommitted means rename completed. The caller can reconcile
// the new bytes and publish them, but must not retry the logical mutation.
var errConfigCommitCommitted = errors.New("config commit completed")

// errConfigCommitUncertain means the post-rename state could not be read
// back. The caller must surface an explicit uncertain outcome and leave the
// next reload to reconcile the on-disk document.
var errConfigCommitUncertain = errors.New("config commit outcome is uncertain")

// errConfigCommitDurabilityUncertain means rename completed but the parent
// directory could not be synced. Visible bytes are not enough to claim a
// crash-durable commit, so callers must surface this outcome explicitly.
var errConfigCommitDurabilityUncertain = errors.New("config commit durability is uncertain")

// ErrConfigHardLink identifies a config file with more than one directory
// entry. Atomic replacement would update only one spelling and leave the
// other hard-link alias stale, so mutable config APIs reject it.
var ErrConfigHardLink = errors.New("config file has multiple hard links")

// errAtomicWriteCommitted means the rename completed and the new bytes are in
// the named destination, but a post-rename durability step failed. Callers
// must not blindly retry such an operation: the logical write already won.
var errAtomicWriteCommitted = errors.New("config write committed with uncertain durability")

// CommitOutcome is the public, byte-free description of a config publication.
// Committed means that the atomic directory-entry update is proven complete;
// MaybeCommitted means that the API outcome cannot prove either side. A
// MaybeCommitted outcome is intentionally distinct from Committed so callers
// cannot retry a mutation as though publication definitely failed.
// Reconciled means a fresh read proved that Path contains the requested bytes.
// Cause retains both the config sentinel and the underlying operation error
// and is available through errors.Is and errors.As via Unwrap.
type CommitOutcome struct {
	Committed      bool
	MaybeCommitted bool
	Reconciled     bool
	Path           string
	Cause          error
}

func (e *CommitOutcome) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause == nil {
		return fmt.Sprintf("config commit outcome for %s", e.Path)
	}
	return fmt.Sprintf("config commit outcome for %s: %v", e.Path, e.Cause)
}

func (e *CommitOutcome) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// CommitOutcomeFromError extracts the public outcome from an error returned
// by a generic or MCP config mutation. It also works when the outcome is
// wrapped by a higher-level lifecycle error.
func CommitOutcomeFromError(err error) (*CommitOutcome, bool) {
	var outcome *CommitOutcome
	if !errors.As(err, &outcome) {
		return nil, false
	}
	return outcome, true
}

var configTestHooks struct {
	sync.Mutex
	beforeOpen            func(string)
	beforeCommitCheck     func()
	afterCommitRename     func() error
	afterCommitRenamePath func(string) error
	// Test-only seam after a successful MCP commit.
	afterMCPCommit     func(string)
	beforeCommitRename func()
	beforeMCPReconcile func(string, []byte)
	forceLinkNoReplace bool
	renameNoReplace    func(int, string, int, string) error
	renameatxNp        func(int, string, int, string, uint32) error
	moveFileEx         func(*uint16, *uint16, uint32) error
	setFileInformation func(uintptr, uint32, *byte, uint32) error
	flushFileBuffers   func(uintptr) error
	unlinkTemp         func(int, string) error
	syncParent         func(string) error
	syncParentFD       func(int) error
	syncParentFile     func(*os.File) error
}

func runConfigBeforeOpenHook(path string) {
	configTestHooks.Lock()
	hook := configTestHooks.beforeOpen
	configTestHooks.Unlock()
	if hook != nil {
		hook(path)
	}
}

func runConfigBeforeCommitCheckHook() {
	configTestHooks.Lock()
	hook := configTestHooks.beforeCommitCheck
	configTestHooks.Unlock()
	if hook != nil {
		hook()
	}
}

func runConfigAfterCommitRenameHook() error {
	configTestHooks.Lock()
	hook := configTestHooks.afterCommitRename
	configTestHooks.Unlock()
	if hook != nil {
		return hook()
	}
	return nil
}

func runConfigAfterCommitRenameHookForPath(path string) error {
	configTestHooks.Lock()
	hookPath := configTestHooks.afterCommitRenamePath
	hook := configTestHooks.afterCommitRename
	configTestHooks.Unlock()
	if hookPath != nil {
		return hookPath(path)
	}
	if hook != nil {
		return hook()
	}
	return nil
}

func runConfigAfterMCPCommitHook(path string) {
	configTestHooks.Lock()
	hook := configTestHooks.afterMCPCommit
	configTestHooks.Unlock()
	if hook != nil {
		hook(path)
	}
}

func runConfigBeforeCommitRenameHook() {
	configTestHooks.Lock()
	hook := configTestHooks.beforeCommitRename
	configTestHooks.Unlock()
	if hook != nil {
		hook()
	}
}

func runConfigBeforeMCPReconcileHook(path string, data []byte) {
	configTestHooks.Lock()
	hook := configTestHooks.beforeMCPReconcile
	configTestHooks.Unlock()
	if hook != nil {
		hook(path, data)
	}
}

func syncConfigParent(path string) error {
	configTestHooks.Lock()
	hook := configTestHooks.syncParent
	configTestHooks.Unlock()
	if hook != nil {
		return hook(path)
	}
	return syncConfigParentOnDisk(path)
}

func syncConfigParentFile(file *os.File) error {
	configTestHooks.Lock()
	hookFD := configTestHooks.syncParentFD
	hookFile := configTestHooks.syncParentFile
	hookPath := configTestHooks.syncParent
	configTestHooks.Unlock()
	if hookFD != nil {
		return hookFD(int(file.Fd()))
	}
	if hookFile != nil {
		return hookFile(file)
	}
	if hookPath != nil {
		// Keep the legacy test seam working. Production commits use the
		// already-open descriptor below and never reopen this pathname.
		return hookPath(file.Name())
	}
	return file.Sync()
}

func sameBytesFingerprint(data []byte, digest [sha256.Size]byte) bool {
	return sha256.Sum256(data) == digest
}

func newCommitOutcome(path string, committed, reconciled bool, causes ...error) error {
	return &CommitOutcome{
		Committed:  committed,
		Reconciled: reconciled,
		Path:       filepath.Clean(path),
		Cause:      errors.Join(causes...),
	}
}

func newMaybeCommitOutcome(path string, causes ...error) error {
	return &CommitOutcome{
		MaybeCommitted: true,
		Path:           filepath.Clean(path),
		Cause:          errors.Join(causes...),
	}
}

func cloneCommitOutcome(outcome *CommitOutcome, reconciled bool) *CommitOutcome {
	if outcome == nil {
		return nil
	}
	clone := *outcome
	clone.Reconciled = reconciled
	return &clone
}

func commitPostCommitCauses(renameErr, hookErr, parentSyncErr error) []error {
	causes := []error{errConfigCommitCommitted}
	if renameErr != nil {
		causes = append(causes, renameErr)
	}
	if hookErr != nil {
		causes = append(causes, hookErr)
	}
	if parentSyncErr != nil {
		causes = append(causes, errAtomicWriteCommitted, errConfigCommitDurabilityUncertain, parentSyncErr)
	}
	return causes
}

func verifySelectedCommitPath(selectedPath, commitPath string) error {
	// This is a boundary check for selecting the physical transaction target,
	// not a claim that an external symlink retarget can be serialized with the
	// later rename. commitPath remains pinned after this check; a later
	// retarget is reported as an uncertain readback outcome when it changes the
	// selected spelling's visible bytes.
	if normalizeReloadPath(selectedPath) != normalizeReloadPath(commitPath) {
		return errConfigCommitVerification
	}
	return nil
}

// atomicWriteFile writes data to a file atomically by writing to a unique
// temporary file in the same directory and renaming it into place. This
// prevents concurrent readers from observing a partially-written file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	path = filepath.Clean(path)
	tmp, err := stageConfigFile(path, data, perm)
	if err != nil {
		return err
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmp)
		}
	}()
	if err := renameConfigTemp(tmp, path, true); err != nil {
		return err
	}
	removeTemp = false
	if err := syncConfigParent(filepath.Dir(path)); err != nil {
		return newCommitOutcome(path, true, false, errAtomicWriteCommitted, errConfigCommitDurabilityUncertain, err)
	}
	return nil
}

func stageConfigFile(path string, data []byte, perm os.FileMode) (string, error) {
	dir := filepath.Dir(filepath.Clean(path))
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	removeTemp = false
	return tmp, nil
}
