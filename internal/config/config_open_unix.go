//go:build !windows

package config

import (
	"fmt"
	"os"
	"syscall"
)

func openStableConfigFile(path string) (*os.File, error) {
	// O_NONBLOCK makes opening a FIFO independent of a writer. The descriptor
	// is still fstat-checked by readStableConfigFileOwned before any read.
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return nil, fmt.Errorf("%w: %s", ErrConfigNonRegular, path)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, os.ErrInvalid
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s", ErrConfigNonRegular, path)
	}
	return file, nil
}

func writeStableConfigFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
