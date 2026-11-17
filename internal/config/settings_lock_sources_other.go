//go:build !windows

package config

import (
	"os/user"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
)

func platformCanonicalGlobalDataPath() string {
	current, err := user.Current()
	if err != nil || current.HomeDir == "" {
		return ""
	}
	// docread.ReadSlotOf selects the active sibling of the logical slot,
	// so a lock stored only in the canonical rush.ktav is visible to protection
	// probes. An already-selected .ktav path passes through unchanged.
	return docread.ReadSlotOf(filepath.Join(current.HomeDir, ".local", "share", "rush", "rush.json"))
}
