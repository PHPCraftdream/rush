//go:build !windows && !linux && !darwin && !freebsd && !openbsd && !netbsd

package procinfo

// tokensSupported: unsupported platform.
const tokensSupported = false

// Probe returns a conservative unknown answer on unsupported platforms.
func Probe(pid int) (alive bool, token string, known bool) {
	return pid > 0, "", false
}

// Alive reports whether pid may be running on this unsupported platform.
func Alive(pid int) bool {
	alive, _, _ := Probe(pid)
	return alive
}

// StartToken is unavailable on this platform.
func StartToken(int) string { return "" }
