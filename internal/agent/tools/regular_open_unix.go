//go:build !windows && !plan9 && !js && !wasip1

package tools

import (
	"context"
	"fmt"
	"os"
	"syscall"
)

func openRegularOSFile(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := verifyRegularHandle(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openRegularAnchorFile(ctx context.Context, anchor *readAnchor) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := anchor.root.Stat(anchor.rootPath())
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file")
	}
	file, err := anchor.root.OpenFile(anchor.rootPath(), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := verifyRegularHandle(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func verifyRegularHandle(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("opened path is not a regular file")
	}
	return nil
}
