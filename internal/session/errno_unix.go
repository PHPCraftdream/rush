//go:build !windows

package session

import "syscall"

// isErrno reports whether err is one of the raw syscall.Errno values in
// want. Errors returned by os.Process signal paths arrive unwrapped, so a
// direct type assertion matches the style previously used in kill_unix.go,
// whose moved copy lives in internal/procinfo.
func isErrno(err error, want ...syscall.Errno) bool {
	errno, ok := err.(syscall.Errno)
	if !ok {
		return false
	}
	for _, w := range want {
		if errno == w {
			return true
		}
	}
	return false
}
