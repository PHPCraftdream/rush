//go:build windows

package tools

import "os"

// fileAllocatedBlocks is the Windows placeholder for the sparse-test guard.
// The sparse test skips on Windows before consulting it.
func fileAllocatedBlocks(fi os.FileInfo) int64 {
	return 1
}
