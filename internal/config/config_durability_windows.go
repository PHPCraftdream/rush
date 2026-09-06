//go:build windows

package config

// Windows has no portable directory fsync equivalent. The staged file is
// flushed before rename, and MoveFileEx receives MOVEFILE_WRITE_THROUGH, so
// this operation deliberately has no additional parent-sync effect or error.
func syncConfigParentOnDisk(string) error { return nil }
