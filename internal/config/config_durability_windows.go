//go:build windows

package config

// Windows does not provide a portable directory fsync equivalent. The file
// contents are flushed before rename; retaining this hook keeps the commit
// sequence identical at its call sites on both platforms.
func syncConfigParent(string) error { return nil }
