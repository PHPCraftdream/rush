//go:build !windows

package fswap

import "os"

func RenameConfigTemp(source, destination string, _ bool) error {
	return os.Rename(source, destination)
}
