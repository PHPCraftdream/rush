//go:build !windows

package fswap

import "os"

func renameConfigTemp(source, destination string, _ bool) error {
	return os.Rename(source, destination)
}
