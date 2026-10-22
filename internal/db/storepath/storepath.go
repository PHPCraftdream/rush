// Package storepath holds the shared store filenames and path helpers.
package storepath

import "path/filepath"

// ActiveFilename is rush_store_v1.db since R12-H4 (batch-only runtime);
// LegacyFilename stays for the importer that reads an old rush.db and
// for the dual-file data-directory hints.
const (
	LegacyFilename             = "rush.db"
	ActiveFilename             = StoreV1Filename
	StoreV1Filename            = "rush_store_v1.db"
	MigrateLockFilename        = "migrate.lock"
	StoreV1MigrateLockFilename = "rush_store_v1.migrate.lock"
)

func DataDirPath(dataDir, filename string) string {
	return filepath.Join(dataDir, filename)
}

func ActivePath(dataDir string) string {
	return DataDirPath(dataDir, ActiveFilename)
}

func StoreV1Path(dataDir string) string {
	return DataDirPath(dataDir, StoreV1Filename)
}
