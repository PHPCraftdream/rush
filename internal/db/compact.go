package db

// Explicit database compaction (SQLite VACUUM) for `rush sessions compact`
// (task #1161). Nothing in this file runs automatically: the only caller is
// the CLI subcommand, and only on an explicit operator request.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
)

// DBStats is a snapshot of the database file's size accounting, read from
// SQLite pragmas plus the on-disk WAL file. FreeBytes is what `sessions gc`
// and `sessions purge` hand back to the freelist without shrinking the
// file; only VACUUM (Compact) returns it to the filesystem.
type DBStats struct {
	PageSize      int64 `json:"page_size"`
	PageCount     int64 `json:"page_count"`
	FreelistCount int64 `json:"freelist_count"`
	// Bytes is PageCount * PageSize: the size of the main database file.
	Bytes int64 `json:"bytes"`
	// FreeBytes is FreelistCount * PageSize: bytes held by the file but
	// not used by any live row.
	FreeBytes int64 `json:"free_bytes"`
	// WALBytes is the current size of the -wal sidecar (0 if absent).
	WALBytes int64 `json:"wal_bytes"`
}

// LiveBytes returns Bytes - FreeBytes: the space live rows actually need
// after a compaction.
func (s DBStats) LiveBytes() int64 {
	return s.Bytes - s.FreeBytes
}

// Stats reads page_size/page_count/freelist_count pragmas and the WAL
// sidecar size off the database behind conn.
func Stats(ctx context.Context, conn *sql.DB) (DBStats, error) {
	var stats DBStats
	if err := conn.QueryRowContext(ctx, "PRAGMA page_size").Scan(&stats.PageSize); err != nil {
		return stats, fmt.Errorf("pragma page_size: %w", err)
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&stats.PageCount); err != nil {
		return stats, fmt.Errorf("pragma page_count: %w", err)
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&stats.FreelistCount); err != nil {
		return stats, fmt.Errorf("pragma freelist_count: %w", err)
	}
	stats.Bytes = stats.PageSize * stats.PageCount
	stats.FreeBytes = stats.PageSize * stats.FreelistCount

	path, err := mainDatabasePath(ctx, conn)
	if err == nil && path != "" {
		// The -wal sidecar may not exist (checkpointed database); a
		// missing file simply means zero WAL bytes.
		if st, statErr := statWAL(path); statErr == nil {
			stats.WALBytes = st
		}
	}
	return stats, nil
}

// mainDatabasePath returns the on-disk path of the "main" database behind
// conn, via PRAGMA database_list. An empty string means the path could not
// be determined (e.g. a :memory: database) — callers treat that as "no WAL
// accounting" rather than an error.
func mainDatabasePath(ctx context.Context, conn *sql.DB) (string, error) {
	rows, err := conn.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", err
		}
		if name == "main" {
			return file, nil
		}
	}
	return "", rows.Err()
}

// CheckpointTruncate runs PRAGMA wal_checkpoint(TRUNCATE): checkpoints the
// WAL back into the main file and truncates the -wal sidecar to zero. It
// returns an error (not silently busy=false) when any other connection
// still holds an open read transaction: that same reader would make an
// immediately following VACUUM fail or leave the file unshrunk, so the
// caller must refuse before touching anything.
func CheckpointTruncate(ctx context.Context, conn *sql.DB) error {
	var busy, logPages, checkpointed int64
	if err := conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logPages, &checkpointed); err != nil {
		return fmt.Errorf("wal_checkpoint(TRUNCATE): %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("wal_checkpoint(TRUNCATE): busy: another connection holds an open transaction; retry when no rush process is using this database")
	}
	return nil
}

// Compact reclaims the database's freelist pages with an in-place VACUUM
// and returns the post-compaction stats. The order matters:
//
//  1. wal_checkpoint(TRUNCATE) with a busy check — refuse when any
//     connection holds an open read transaction (the caller surfaces this
//     as a refusal; nothing has been touched yet).
//  2. temp_store=FILE for the VACUUM itself: the in-database default is
//     MEMORY (see connect.go's pragma table) and the temp copy of live
//     pages must not be capped by RAM. Restored via defer.
//  3. VACUUM — in-place and transactional: a crash mid-way leaves the
//     database in its old or new shape, recoverable through the WAL by
//     the next opener. Deliberately not "VACUUM INTO + rename": renaming
//     over a file other processes hold open is not atomic and fails on
//     Windows.
//  4. wal_checkpoint(TRUNCATE) again: in WAL mode the VACUUM's rewritten
//     pages land in the WAL and the main file only shrinks at the
//     checkpoint. Without this step the file stays big on disk.
//
// On any error the database is left in its pre-Compact state (VACUUM is
// transactional); the caller releases locks and restores pragmas on its
// side via defer.
func Compact(ctx context.Context, conn *sql.DB) (DBStats, error) {
	var stats DBStats
	if err := CheckpointTruncate(ctx, conn); err != nil {
		return stats, err
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA temp_store=FILE"); err != nil {
		return stats, fmt.Errorf("pragma temp_store=FILE: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA temp_store=MEMORY")
	}()
	if _, err := conn.ExecContext(ctx, "VACUUM"); err != nil {
		return stats, fmt.Errorf("vacuum: %w", err)
	}
	// The main file only shrinks when the WAL the VACUUM wrote is
	// checkpointed back and truncated; skipping this step leaves the
	// freed space in the -wal sidecar instead of the filesystem.
	if err := CheckpointTruncate(ctx, conn); err != nil {
		return stats, err
	}
	return Stats(ctx, conn)
}

// IntegrityCheck runs PRAGMA integrity_check and returns the verdict
// ("ok" on a healthy database). Test helper surface; also usable
// interactively after a compaction.
func IntegrityCheck(ctx context.Context, conn *sql.DB) (string, error) {
	var verdict strings.Builder
	rows, err := conn.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		if verdict.Len() > 0 {
			verdict.WriteString("; ")
		}
		verdict.WriteString(line)
	}
	return verdict.String(), rows.Err()
}

// statWAL returns the size in bytes of the WAL sidecar belonging to the
// database file at dbPath (0 when the sidecar does not exist).
func statWAL(dbPath string) (int64, error) {
	st, err := os.Stat(dbPath + "-wal")
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return st.Size(), nil
}
