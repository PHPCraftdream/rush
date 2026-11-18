//go:build windows

package cliproc

// keepBrokenPipeAsError is a no-op on Windows: a write to a closed pipe
// already returns an error there.
func KeepBrokenPipeAsError() (restore func()) {
	return func() {}
}
