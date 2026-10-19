//go:build !windows

package app

import (
	"fmt"
	"os"
	"syscall"
)

// hostFileIdentity reports a stable per-file identity (the stat device and
// inode) for the given path, so diagnostics can tell whether a lock file was
// replaced by a different file or merely went missing. It returns a
// "stat-error: ..." string when the path cannot be stat'ed and "unsupported"
// when the platform's Sys() payload is not a *syscall.Stat_t.
func hostFileIdentity(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "stat-error: " + err.Error()
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "unsupported"
	}
	return fmt.Sprintf("dev:%d ino:%d", st.Dev, st.Ino)
}
