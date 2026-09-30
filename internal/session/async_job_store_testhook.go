package session

import "github.com/PHPCraftdream/rush/internal/db"

// WrapReadQueriesForTest routes this store's reader queries through
// wrap(writer connection), so a test in another package can count them (the
// query-count contract of LiveWorkForRoots). Test-only: no production path
// calls it. Reading through the writer connection is equivalent for a test
// (same database, WAL).
func (s *AsyncJobStore) WrapReadQueriesForTest(wrap func(db.DBTX) db.DBTX) {
	s.readQ.Store(db.New(wrap(s.sqlDB)))
}

// ReleaseRetainedHostLocksForTest releases every host lock a forced shutdown
// pinned (CloseKeepLock) and forgets the given own-host ids, so a test that
// exercised the forced path can clean up its data dir. Test-only.
func ReleaseRetainedHostLocksForTest(hostIDs ...string) {
	releaseRetainedHostLocksForTest()
	for _, id := range hostIDs {
		unmarkOwnHostID(id)
	}
}
