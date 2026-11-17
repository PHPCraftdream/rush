//go:build windows

package settings

import (
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"golang.org/x/sys/windows"
)

func platformCanonicalGlobalDataPath() string {
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil || localAppData == "" {
		return ""
	}
	// docread.ReadSlotOf selects the active sibling of the logical slot, so a lock
	// stored only in the canonical rush.ktav is visible to protection
	// probes. An already-selected .ktav path passes through unchanged.
	return docread.ReadSlotOf(filepath.Join(localAppData, cfgmodel.AppName, "rush.json"))
}
