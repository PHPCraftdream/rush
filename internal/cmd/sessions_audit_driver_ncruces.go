//go:build !((darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (windows && (386 || amd64 || arm64)))

package cmd

// Read-only SQLite handle for `rush sessions audit` on platforms whose
// internal/db build uses the ncruces driver. The build tag is the exact
// complement of the one internal/db/connect_modernc.go uses, so the two
// drivers are never both compiled (and therefore never both registered as
// "sqlite") in one binary.

import (
	"database/sql"
	"fmt"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
)

// openAuditRO opens dbPath for reading only. Same contract as the modernc
// twin in sessions_audit_driver_modernc.go: mode=ro makes SQLite refuse
// writes outright instead of silently taking a live writer's place, and
// _txlock=deferred is the read-only counterpart to the writer's
// _txlock=immediate. No journal_mode pragma is set here either — it would
// need a write the read-only connection cannot perform.
func openAuditRO(dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_txlock=deferred&mode=ro", dbPath)
	db, err := driver.Open(dsn, func(c *sqlite3.Conn) error {
		// query_only is defence in depth on top of mode=ro, set per
		// connection because ncruces opens one per pool slot.
		if err := c.Exec("PRAGMA query_only = ON;"); err != nil {
			// A driver that refuses the pragma still has mode=ro behind
			// it, so a failure here is not fatal.
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open database read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
