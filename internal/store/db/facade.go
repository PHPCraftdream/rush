package db

import (
	"context"
	"database/sql"

	"github.com/PHPCraftdream/rush/internal/store/db/dbconn"
	"github.com/PHPCraftdream/rush/internal/store/db/migrater"
)

// This file is the pool facade (design
// docs/plans/2026-11-15-sprint0-split-db.md sections 3.1-3.3): consumers of
// package db keep their db.Connect/db.Release entry points unchanged while
// the pool itself lives in dbconn and the migration machinery lives in
// migrater. There is no logic here beyond forwarding.

// Facade aliases for the migration options: thin re-exports of migrater's
// option types — a value returned by db.WithMayMigrate is a
// migrater.MigrateOption and is accepted by dbconn.Connect and
// migrater.ApplyLocked without conversion.

type MigrateOption = migrater.MigrateOption

type PreMigrationCheck = migrater.PreMigrationCheck

// BusyTimeout re-exports migrater's statement timeout constant for
// existing consumers.
const BusyTimeout = migrater.BusyTimeout

func WithMayMigrate(v bool) MigrateOption { return migrater.WithMayMigrate(v) }

func WithMigrationLockPath(p string) MigrateOption { return migrater.WithMigrationLockPath(p) }

func WithPreMigrationChecks(checks ...PreMigrationCheck) MigrateOption {
	return migrater.WithPreMigrationChecks(checks...)
}

// Connect opens (or reuses) the pooled SQLite writer connection for dataDir
// and runs migrations exactly once per database file. Every open is paired
// with a ReleaseConn of the returned handle; see dbconn.Connect for the
// full pool contract (shared refCount, single-connection writer, reader
// fallback).
func Connect(ctx context.Context, dataDir string, migrationOpts ...MigrateOption) (*sql.DB, error) {
	return dbconn.Connect(ctx, dataDir, poolOpenOptions(migrationOpts)...)
}

// ConnectRead opens (or reuses) the read-only pool for the same database
// Connect opens, sharing its reference count; see dbconn.ConnectRead.
func ConnectRead(ctx context.Context, dataDir string, migrationOpts ...MigrateOption) (*sql.DB, error) {
	return dbconn.ConnectRead(ctx, dataDir, poolOpenOptions(migrationOpts)...)
}

// Release decrements the reference count for the database at dataDir; the
// pooled handles close when it reaches zero.
func Release(dataDir string) error { return dbconn.Release(dataDir) }

// ReleaseConn decrements the reference count of the pooled generation that
// owns conn; the token semantics are documented on dbconn.ReleaseConn.
func ReleaseConn(conn *sql.DB) error { return dbconn.ReleaseConn(conn) }

// ReleaseAll forcibly closes the pooled connection for dataDir regardless of
// its current reference count; see dbconn.ReleaseAll.
func ReleaseAll(dataDir string) error { return dbconn.ReleaseAll(dataDir) }

// ResetPool establishes a pool-wide lifecycle barrier, closes all pooled
// connections, and clears the pool; see dbconn.ResetPool.
func ResetPool() { dbconn.ResetPool() }

// poolOpenOptions builds the dbconn open option list for every pool open
// through this facade: the prepared-statement cache drop (pdaDropBatchStmts)
// must run while the path lock is held, before the pooled handles close —
// the same behavior closeEntry had when the pool lived in this package —
// and the caller's migration options ride along unchanged.
func poolOpenOptions(migrationOpts []MigrateOption) []dbconn.Option {
	opts := make([]dbconn.Option, 0, len(migrationOpts)+1)
	opts = append(opts, dbconn.WithCloseAssist(pdaDropBatchStmts))
	return append(opts, dbconn.WithMigrateOptions(migrationOpts...))
}
