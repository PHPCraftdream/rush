//go:build !windows

package config

import (
	"os"
	"path/filepath"
)

func configParentIdentity(path string) configFileIdentity {
	info, err := os.Stat(filepath.Dir(filepath.Clean(path)))
	if err != nil {
		return configFileIdentity{}
	}
	return configFileIdentityOf(info)
}
