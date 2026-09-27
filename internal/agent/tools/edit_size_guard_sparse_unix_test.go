//go:build unix

package tools

import (
	"os"
	"syscall"
)

// fileAllocatedBlocks reports how many 512-byte blocks a file occupies, so a
// sparse-file test can skip on filesystems that materialized the extension
// instead of leaving it sparse. Returns 1 when the platform type is
// unavailable, treating the file as allocated.
func fileAllocatedBlocks(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks
	}
	return 1
}
