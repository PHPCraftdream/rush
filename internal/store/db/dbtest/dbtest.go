//go:build (darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (windows && (386 || amd64 || arm64))

// Package dbtest opens throwaway SQLite databases for tests with the pragmas
// production uses (journal_mode=WAL, synchronous=NORMAL; see internal/db).
//
// The driver's defaults (rollback journal, synchronous=FULL) fsync several
// times per commit: about 2.5 ms per commit against about 0.02 ms here on a
// healthy disk, and seconds per commit when a CI disk stalls, which is what
// turned fixture setup and every queue operation into a timing hazard. A test
// fixture does not need durability across power loss.
package dbtest

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

var uriEscaper = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

// DSN returns the data source name of the database file at path with the
// fixture pragmas applied.
func DSN(path string) string {
	return "file:" + uriEscaper.Replace(filepath.ToSlash(path)) + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
}

// Open opens the database file at path with the fixture pragmas applied.
func Open(path string) (*sql.DB, error) {
	return sql.Open("sqlite", DSN(path))
}

// OpenFK opens the database file at path with the fixture pragmas plus
// foreign_keys(ON), the cascade behaviour production enables.
func OpenFK(path string) (*sql.DB, error) {
	return sql.Open("sqlite", DSN(path)+"&_pragma=foreign_keys(ON)")
}

// OpenSingle opens the database at path through OpenFK with a single
// pooled connection, mirroring what the migrater fixture hands to
// migrations, and closes it at test cleanup.
func OpenSingle(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := OpenFK(path)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
