//go:build linux

package procinfo

// tokensSupported: /proc/<pid>/stat exposes start ticks.

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const tokensSupported = true

// Probe returns liveness, the proc start token, and whether both are known.
// EPERM => alive but unknown; unreadable stat => alive but unknown token.
func Probe(pid int) (alive bool, token string, known bool) {
	if pid <= 0 {
		return false, "", true
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		if errors.Is(err, syscall.ESRCH) {
			return false, "", true
		}
		return true, "", false
	}
	b, e := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if e != nil {
		return true, "", false
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 {
		f := strings.Fields(s[i+1:])
		if len(f) > 19 {
			return true, f[19], true
		}
	}
	return true, "", false
}

// Alive reports whether pid identifies a running process.
func Alive(pid int) bool {
	a, _, _ := Probe(pid)
	return a
}

// StartToken returns Linux proc start ticks, or "" if unknown.
func StartToken(pid int) string {
	_, t, _ := Probe(pid)
	return t
}
