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
	// readSlotOf selects the active sibling of the logical slot, so a lock
	// stored only in the canonical rush.ktav is visible to protection
	// probes. An already-selected .ktav path passes through unchanged.
	return readSlotOf(filepath.Join(localAppData, "rush", "rush.json"))
}
