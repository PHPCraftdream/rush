//go:build !windows

package config

import (
	"os/user"
	"path/filepath"
)

func platformCanonicalGlobalDataPath() string {
	current, err := user.Current()
	if err != nil || current.HomeDir == "" {
		return ""
	}
	return filepath.Join(current.HomeDir, ".local", "share", "rush", "rush.json")
}
