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
