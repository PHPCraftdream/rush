package slots

// Lightweight, format-aware readers over the global config slot for CLI
// paths that must not initialize the application: no store, no DB, no
// migration. Callers decide what an error means (e.g. os.IsNotExist means
// "no document").

import (
	"os"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/slotmigrate"
)

// ktavAutoMigrate is the KTAV-4 gate storage in slots (the graph root);
// slotmigrate keeps a private mirror synced only via the test seam below.
var ktavAutoMigrate = true

// ReadConfigDocumentLight reads the document for the logical slot named by
// legacyPath without initializing the application: no store, no DB, no
// migration. It selects the active physical file (rush.ktav when present,
// otherwise rush.json), fails closed via docread.CheckBackupOnlySlot when only a
// backup exists, and decodes .ktav bytes into JSON-shaped bytes. JSON
// documents are returned unchanged. Errors are returned as-is; the caller
// distinguishes os.IsNotExist as "no document".
func ReadConfigDocumentLight(legacyPath string) ([]byte, error) {
	legacy := docread.LegacySlotOf(legacyPath)
	if err := docread.CheckBackupOnlySlot(legacy); err != nil {
		return nil, err
	}
	selected := docread.SelectConfigSlot(legacy)
	data, err := os.ReadFile(selected)
	if err != nil {
		return nil, err
	}
	if docread.IsKtavConfigPath(selected) {
		return docread.DecodeConfigDocument(selected, data)
	}
	return data, nil
}

// ActiveConfigFileName returns the selected physical path of the logical
// config slot named by legacyPath (rush.ktav vs rush.json), so CLI help and
// error text can name the real file without hardcoding a spelling. An empty
// legacyPath returns "".
func ActiveConfigFileName(legacyPath string) string {
	return docread.SelectConfigSlot(docread.LegacySlotOf(legacyPath))
}

// SetKtavAutoMigrateForTest is a test seam ONLY: it sets the package-level
// ktavAutoMigrate flag AND slotmigrate's private mirror, and returns a func
// restoring both previous values.
func SetKtavAutoMigrateForTest(on bool) (restore func()) {
	prev := ktavAutoMigrate
	ktavAutoMigrate = on
	restoreSlotmigrate := slotmigrate.SetKtavAutoMigrateForTest(on)
	return func() {
		ktavAutoMigrate = prev
		restoreSlotmigrate()
	}
}

// KtavAutoMigrateEnabled reports whether newly created config files default
// to the Ktav spelling (the KTAV-4 gate). Read-only: callers such as the
// migrate command need the gate value itself, not the opaque test seam.
func KtavAutoMigrateEnabled() bool { return ktavAutoMigrate }
