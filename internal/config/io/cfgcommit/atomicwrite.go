package cfgcommit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/io/cfghooks"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
	"github.com/PHPCraftdream/rush/internal/config/io/fswap"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filelock"
)

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

func runConfigBeforeOpenHook(path string) {
	if hook := cfghooks.Snapshot().BeforeOpen; hook != nil {
		hook(path)
	}
}

func runConfigAfterStableReadHook(path string) {
	if hook := cfghooks.Snapshot().AfterStableRead; hook != nil {
		hook(path)
	}
}

func runConfigBeforeCommitCheckHook() {
	if hook := cfghooks.Snapshot().BeforeCommitCheck; hook != nil {
		hook()
	}
}

func runConfigAfterCommitRenameHook() error {
	if hook := cfghooks.Snapshot().AfterCommitRename; hook != nil {
		return hook()
	}
	return nil
}

func runConfigAfterCommitRenameHookForPath(path string) error {
	hooks := cfghooks.Snapshot()
	if hooks.AfterCommitRenamePath != nil {
		return hooks.AfterCommitRenamePath(path)
	}
	if hooks.AfterCommitRename != nil {
		return hooks.AfterCommitRename()
	}
	return nil
}

func runConfigAfterMCPCommitHook(path string) {
	if hook := cfghooks.Snapshot().AfterMCPCommit; hook != nil {
		hook(path)
	}
}

func runConfigBeforeCommitRenameHook() {
	if hook := cfghooks.Snapshot().BeforeCommitRename; hook != nil {
		hook()
	}
}

func runConfigBeforeMCPReconcileHook(path string, data []byte) {
	if hook := cfghooks.Snapshot().BeforeMCPReconcile; hook != nil {
		hook(path, data)
	}
}

func acquireConfigFileLock(ctx context.Context, path string) (*filelock.FileLock, error) {
	if hook := cfghooks.Snapshot().AcquireConfigLock; hook != nil {
		return hook(ctx, path)
	}
	return filelock.AcquireFileLockContext(ctx, path)
}

func releaseConfigFileLock(path string, lock *filelock.FileLock) error {
	if hook := cfghooks.Snapshot().ReleaseConfigLock; hook != nil {
		return hook(path, lock)
	}
	return lock.Release()
}

func configContextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if hook := cfghooks.Snapshot().WithConfigTimeout; hook != nil {
		return hook(parent, timeout)
	}
	return context.WithTimeout(parent, timeout)
}

func syncConfigParent(path string) error {
	if hook := cfghooks.Snapshot().SyncParent; hook != nil {
		return hook(path)
	}
	return syncConfigParentOnDisk(path)
}

// SyncConfigParentFile flushes an already-open parent directory descriptor.
func SyncConfigParentFile(file *os.File) error {
	return syncConfigParentFile(file)
}

func syncConfigParentFile(file *os.File) error {
	hooks := cfghooks.Snapshot()
	if hooks.SyncParentFD != nil {
		return hooks.SyncParentFD(int(file.Fd()))
	}
	if hooks.SyncParentFile != nil {
		return hooks.SyncParentFile(file)
	}
	if hooks.SyncParent != nil {
		// Keep the legacy test seam working. Production commits use the
		// already-open descriptor below and never reopen this pathname.
		return hooks.SyncParent(file.Name())
	}
	return file.Sync()
}

// RunConfigAfterMCPCommitHook invokes the root test seam after a successful
// MCP commit (io layer that consumes it directly in a later pass: root).
func RunConfigAfterMCPCommitHook(path string) { runConfigAfterMCPCommitHook(path) }

// RunConfigBeforeMCPReconcileHook invokes the root test seam before MCP
// reconciliation (io layer: root).
func RunConfigBeforeMCPReconcileHook(path string, data []byte) {
	runConfigBeforeMCPReconcileHook(path, data)
}

// AcquireConfigFileLock acquires the sidecar config lock, honoring the test
// seam (io layer: root).
func AcquireConfigFileLock(ctx context.Context, path string) (*filelock.FileLock, error) {
	return acquireConfigFileLock(ctx, path)
}

// ReleaseConfigFileLock releases the sidecar config lock, honoring the test
// seam (io layer: root).
func ReleaseConfigFileLock(path string, lock *filelock.FileLock) error {
	return releaseConfigFileLock(path, lock)
}

// ConfigContextWithTimeout derives a context honoring the test seam (io
// layer: root).
func ConfigContextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return configContextWithTimeout(parent, timeout)
}

// SyncConfigParentOnDisk flushes the parent directory of path to stable
// storage without any test seam (io layer: root).
func SyncConfigParentOnDisk(path string) error { return syncConfigParentOnDisk(path) }

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
	if docread.NormalizeReloadPath(selectedPath) != docread.NormalizeReloadPath(commitPath) &&
		!caseEquivalentConfigPaths(selectedPath, commitPath) {
		return fileid.ErrConfigCommitVerification()
	}
	return nil
}

func caseEquivalentConfigPaths(selectedPath, commitPath string) bool {
	if !configEquivalentLeafNames(filepath.Dir(commitPath), filepath.Base(selectedPath), filepath.Base(commitPath)) {
		return false
	}
	selectedParent, commitParent := fileid.ConfigParentIdentity(selectedPath), fileid.ConfigParentIdentity(commitPath)
	return selectedParent.Valid && selectedParent == commitParent &&
		configDirectoryIsCaseInsensitive(filepath.Dir(commitPath))
}

// AtomicWriteFile writes data to path using the atomic config commit implementation.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return atomicWriteFile(path, data, perm)
}

// SyncConfigParent flushes the parent directory of path to stable storage.
func SyncConfigParent(path string) error {
	return syncConfigParent(path)
}

// NewCommitOutcome builds a public CommitOutcome error from commit causes.
func NewCommitOutcome(path string, committed, reconciled bool, causes ...error) error {
	return newCommitOutcome(path, committed, reconciled, causes...)
}

// CommitConfigFile verifies and publishes the requested bytes through the
// platform atomic commit implementation.
func CommitConfigFile(selectedPath, commitPath string, data []byte, perm os.FileMode, expected docread.Fingerprint, expectedOwner int, enforceOwner bool, migrationHooks ...func(string) error) (docread.Fingerprint, error) {
	return commitConfigFile(selectedPath, commitPath, data, perm, expected, expectedOwner, enforceOwner, migrationHooks...)
}

// ErrConfigCommitCommitted reports that the rename completed.
func ErrConfigCommitCommitted() error { return errConfigCommitCommitted }

// ErrConfigCommitUncertain reports an unreadable post-rename state.
func ErrConfigCommitUncertain() error { return errConfigCommitUncertain }

// ErrConfigCommitDurabilityUncertain reports a commit without a synced parent.
func ErrConfigCommitDurabilityUncertain() error { return errConfigCommitDurabilityUncertain }

// ErrAtomicWriteCommitted reports a committed write with uncertain durability.
func ErrAtomicWriteCommitted() error { return errAtomicWriteCommitted }

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
	if err := fswap.RenameConfigTemp(tmp, path, true); err != nil {
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

// CloneCommitOutcome copies a public outcome, setting Reconciled (io layer
// that will consume it directly in a later pass: root).
func CloneCommitOutcome(outcome *CommitOutcome, reconciled bool) *CommitOutcome {
	return cloneCommitOutcome(outcome, reconciled)
}
