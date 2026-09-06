//go:build windows

package config

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

func openStableConfigFile(path string) (*os.File, error) {
	// Follow the same target-resolution semantics as os.Open, while allowing a
	// concurrent handle-relative namespace mutation to proceed.
	return openWindowsConfigHandle(path, windows.GENERIC_READ, 0)
}

func writeStableConfigFile(path string, data []byte) error {
	file, err := openWindowsConfigHandle(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}
