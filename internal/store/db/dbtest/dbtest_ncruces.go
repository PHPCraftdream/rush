//go:build !((darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (windows && (386 || amd64 || arm64)))

package dbtest

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
)

// OpenFK opens the database file at path with the fixture pragmas plus
// foreign_keys(ON) on the platforms where the production driver is
// ncruces/go-sqlite3 (the modernc driver does not build there). It mirrors
// the modernc variant so that package db tests compile and lint on every
// platform the package supports.
func OpenFK(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_txlock=immediate", path)
	return driver.Open(dsn, func(c *sqlite3.Conn) error {
		for _, pragma := range []string{"journal_mode = WAL", "synchronous = NORMAL", "foreign_keys = ON"} {
			if err := c.Exec("PRAGMA " + pragma + ";"); err != nil {
				return fmt.Errorf("failed to set pragma %q: %w", pragma, err)
			}
		}
		return nil
	})
}

// OpenSingle opens the database at path through OpenFK with a single
// pooled connection and closes it at test cleanup.
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
