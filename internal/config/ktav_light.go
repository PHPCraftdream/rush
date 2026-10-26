package config

// Lightweight, format-aware readers over the global config slot for CLI
// paths that must not initialize the application: no store, no DB, no
// migration. Callers decide what an error means (e.g. os.IsNotExist means
// "no document").

import (
	"os"
)

// ReadConfigDocumentLight reads the document for the logical slot named by
// legacyPath without initializing the application: no store, no DB, no
// migration. It selects the active physical file (rush.ktav when present,
// otherwise rush.json), fails closed via checkBackupOnlySlot when only a
// backup exists, and decodes .ktav bytes into JSON-shaped bytes. JSON
// documents are returned unchanged. Errors are returned as-is; the caller
// distinguishes os.IsNotExist as "no document".
func ReadConfigDocumentLight(legacyPath string) ([]byte, error) {
	legacy := legacySlotOf(legacyPath)
	if err := checkBackupOnlySlot(legacy); err != nil {
		return nil, err
	}
	selected := selectConfigSlot(legacy)
	data, err := os.ReadFile(selected)
	if err != nil {
		return nil, err
	}
	if isKtavConfigPath(selected) {
		return decodeConfigDocument(selected, data)
	}
	return data, nil
}

// ActiveConfigFileName returns the selected physical path of the logical
// config slot named by legacyPath (rush.ktav vs rush.json), so CLI help and
// error text can name the real file without hardcoding a spelling. An empty
// legacyPath returns "".
func ActiveConfigFileName(legacyPath string) string {
	return selectConfigSlot(legacySlotOf(legacyPath))
}

// test seam
// SetKtavAutoMigrateForTest is a test seam ONLY: it sets the package-level
// ktavAutoMigrate flag and returns a func restoring the previous value.
func SetKtavAutoMigrateForTest(on bool) (restore func()) {
	prev := ktavAutoMigrate
	ktavAutoMigrate = on
	return func() { ktavAutoMigrate = prev }
}

// KtavAutoMigrateEnabled reports whether newly created config files default
// to the Ktav spelling (the KTAV-4 gate). Read-only: callers such as the
// migrate command need the gate value itself, not the opaque test seam.
func KtavAutoMigrateEnabled() bool { return ktavAutoMigrate }
