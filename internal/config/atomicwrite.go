package config

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

var errConfigCommitVerification = errors.New("config destination could not be verified immediately before commit")

var configTestHooks struct {
	sync.Mutex
	beforeOpen        func(string)
	beforeCommitCheck func()
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
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
