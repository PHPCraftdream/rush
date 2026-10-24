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
	// readSlotOf selects the active sibling of the logical slot, so a lock
	// stored only in the canonical rush.ktav is visible to protection
	// probes. An already-selected .ktav path passes through unchanged.
	return readSlotOf(filepath.Join(current.HomeDir, ".local", "share", "rush", "rush.json"))
}
