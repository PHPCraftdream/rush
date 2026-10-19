//go:build windows

package app

// hostFileIdentity has no dev/ino equivalent on Windows, so diagnostics
// report the identity as unavailable there.
func hostFileIdentity(_ string) string {
	return "n/a"
}
