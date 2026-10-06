//go:build !windows && !linux

package audit

// parentProcessName has no lookup outside Windows and Linux.
func parentProcessName(int) string { return "" }
