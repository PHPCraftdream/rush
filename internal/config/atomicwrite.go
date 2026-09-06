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

// errAtomicWriteCommitted means the rename completed and the new bytes are in
// the named destination, but a post-rename durability step failed. Callers
// must not blindly retry such an operation: the logical write already won.
var errAtomicWriteCommitted = errors.New("config write committed with uncertain durability")

var configTestHooks struct {
	sync.Mutex
	beforeOpen        func(string)
	beforeCommitCheck func()
	afterCommitRename func() error
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

func sameBytesFingerprint(data []byte, digest [sha256.Size]byte) bool {
	return sha256.Sum256(data) == digest
}

// atomicWriteFile writes data to a file atomically by writing to a unique
// temporary file in the same directory and renaming it into place. This
// prevents concurrent readers from observing a partially-written file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := syncConfigParent(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: sync config parent: %v", errAtomicWriteCommitted, err)
	}
	return nil
}
