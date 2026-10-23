package session

// ReleaseRetainedHostLocksForTest releases every host lock a forced shutdown
// pinned (CloseKeepLock) and forgets the given own-host ids, so a test that
// exercised the forced path can clean up its data dir. Test-only.
func ReleaseRetainedHostLocksForTest(hostIDs ...string) {
	releaseRetainedHostLocksForTest()
	for _, id := range hostIDs {
		unmarkOwnHostID(id)
	}
}
