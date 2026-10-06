//go:build darwin || freebsd || openbsd || netbsd

package procinfo

import "syscall"

// tokensSupported: no portable start token on these platforms.
const tokensSupported = false

// Probe conservatively reports liveness where start tokens are unavailable.
// Liveness is known; tokens are always unknown on these platforms.
func Probe(pid int) (alive bool, token string, known bool) {
	if pid <= 0 {
		return false, "", true
	}
	err := syscall.Kill(pid, 0)
	if err == nil || err == syscall.EPERM {
		return true, "", false
	}
	if err == syscall.ESRCH {
		return false, "", true
	}
	return true, "", false
}

// Alive reports whether pid identifies a running process.
func Alive(pid int) bool {
	alive, _, _ := Probe(pid)
	return alive
}

// StartToken is unavailable on these platforms.
func StartToken(int) string { return "" }
