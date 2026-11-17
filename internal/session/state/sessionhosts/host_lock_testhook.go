package sessionhosts

import (
	"database/sql"

	"github.com/PHPCraftdream/rush/internal/store/db"
)

// SQLDBForTest exposes the writer connection to external test packages
// (package session's finalization hold tests open real transactions on a
// store's single-connection pool). Test-only.
func (s *HostStore) SQLDBForTest() *sql.DB { return s.sqlDB }

// ReleaseRetainedHostLocksForTest releases every host lock a forced shutdown
// pinned (CloseKeepLock) and forgets the given own-host ids, so a test that
// exercised the forced path can clean up its data dir. Test-only.
func ReleaseRetainedHostLocksForTest(hostIDs ...string) {
	releaseRetainedHostLocksForTest()
	for _, id := range hostIDs {
		unmarkOwnHostID(id)
	}
}

// MarkOwnHostIDForTest / UnmarkOwnHostIDForTest expose the own-host id
// registry to external test packages (package session's recovery tests
// marked their own hosts before these helpers moved here with the host
// lock). Test-only.
func MarkOwnHostIDForTest(id string)   { markOwnHostID(id) }
func UnmarkOwnHostIDForTest(id string) { unmarkOwnHostID(id) }

// AsyncHostStoreFromQueries adapts an already-built *db.Queries onto
// AsyncHostStore for external test packages (*db.Queries itself does not
// satisfy db.DBTX, so AsyncHostStoreFromConn cannot take it). Test-only.
func AsyncHostStoreFromQueries(q *db.Queries) AsyncHostStore {
	return hostNodeStore{q: q}
}

// QueriesForTest exposes the store's writer *db.Queries to external test
// packages (package session's finalization hold tests route RegisterHost/
// Close through it). Test-only.
func (s *HostStore) QueriesForTest() *db.Queries { return s.q }
