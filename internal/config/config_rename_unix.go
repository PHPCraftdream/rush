//go:build !windows

package config

import "os"

func renameConfigTemp(source, destination string, _ bool) error {
	return os.Rename(source, destination)
}
