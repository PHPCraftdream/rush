//go:build (darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (windows && (386 || amd64 || arm64))

package cmd

// Read-only SQLite handle for `rush sessions audit`. The build tag is the
// exact one internal/db/connect_modernc.go uses: the split has to stay
// identical so the "sqlite" driver is registered exactly once per build
// (this package's other files already pull in internal/db, which
// blank-imports modernc.org/sqlite).

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// openAuditRO opens dbPath for reading only.
//
// mode=ro is the load-bearing part: it asks SQLite to refuse writes outright
// (SQLITE_READONLY) rather than silently taking a live writer's place, and
// it applies to every pooled connection. _txlock=deferred is the read-only
// counterpart to the writer's _txlock=immediate — a reader acquires no
// reserved lock up front. Unlike internal/db's openReadDB, no journal_mode
// pragma is sent: setting it would require a write the read-only
// connection cannot perform.
//
// PRAGMA query_only is defence in depth on top of mode=ro, so the pool is
// capped at one connection: a database/sql pool hands out arbitrary
// connections, and a pragma set on one of them guards only that one.
func openAuditRO(dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_txlock=deferred", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	// A driver that refuses the pragma still has mode=ro behind it, so a
	// failure here is not fatal and is deliberately ignored.
	if _, err := db.ExecContext(context.Background(), "PRAGMA query_only = ON;"); err != nil {
		_ = err
	}
	return db, nil
}
