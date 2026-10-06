//go:build windows

package config

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

func platformCanonicalGlobalDataPath() string {
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil || localAppData == "" {
		return ""
	}
	return filepath.Join(localAppData, "rush", "rush.json")
}
