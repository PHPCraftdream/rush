package settings

import "testing"

// SetCanonicalGlobalDataPathForTest replaces the unexported canonicalGlobalDataPath provider for one test and restores it via t.Cleanup. Test-only seam; production code must never call it.
func SetCanonicalGlobalDataPathForTest(t testing.TB, path string) {
	previous := canonicalGlobalDataPath
	canonicalGlobalDataPath = func() string { return path }
	t.Cleanup(func() { canonicalGlobalDataPath = previous })
}
