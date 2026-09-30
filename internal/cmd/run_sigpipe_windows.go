//go:build windows

package cmd

// keepBrokenPipeAsError is a no-op on Windows: a write to a closed pipe
// already returns an error there.
func keepBrokenPipeAsError() (restore func()) {
	return func() {}
}
