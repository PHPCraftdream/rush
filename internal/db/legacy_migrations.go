package db

import (
	"embed"
	"io/fs"
)

// legacyMigrations is the importer-owned copy of the pre-store-v1
// migration chain. ImportLegacyStore upgrades a private snapshot of an
// old rush.db to this chain's head before copying rows, so data written
// by older binaries (model-slot renames, cost backfills, tables that do
// not exist yet) arrives in the new store instead of being silently
// dropped.
//
// H4 will replace the active embedded set (internal/db/migrations,
// embedded as FS in connect.go) with the fresh store_v1 schema. This
// copy keeps the importer able to upgrade legacy files after that swap:
// it is frozen at the last legacy head and must never gain a store_v1
// migration. Until H4 lands, TestLegacyImportMigrationsMatchEmbedded
// pins this directory byte-identical to internal/db/migrations; H4
// deletes that test together with the migrations directory and
// repoints sqlc.yaml.
//
//go:embed legacymigrations/*.sql
var legacyMigrations embed.FS

// legacyMigrationFS returns the legacy chain rooted at its own
// directory, so callers see the bare migration filenames. It mirrors
// migrationFS for the active set.
func legacyMigrationFS() (fs.FS, error) {
	return fs.Sub(legacyMigrations, "legacymigrations")
}
