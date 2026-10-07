//go:build windows

package procinfo

// tokensSupported: Windows exposes process creation times.

import (
	"strconv"

	"golang.org/x/sys/windows"
)

const tokensSupported = true

// openProcess is swappable so tests can force ERROR_ACCESS_DENIED: an
// elevated runner may open even the System process.
var openProcess = windows.OpenProcess

// Probe reports whether pid is alive and its creation token when readable.
// ERROR_ACCESS_DENIED => alive=true, token="", known=false.
func Probe(pid int) (alive bool, token string, known bool) {
	if pid <= 0 {
		return false, "", true
	}
	h, e := openProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if e != nil {
		if e == windows.ERROR_ACCESS_DENIED {
			return true, "", false
		}
		return false, "", true
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return true, "", false
	}
	if code != 259 {
		return false, "", true
	}
	var c, x, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &x, &k, &u) != nil {
		return true, "", false
	}
	return true, strconv.FormatUint(uint64(c.HighDateTime)<<32|uint64(c.LowDateTime), 10), true
}

// Alive reports whether pid identifies a running process.
func Alive(pid int) bool {
	a, _, _ := Probe(pid)
	return a
}

// StartToken returns the process creation time, or "" if unknown.
func StartToken(pid int) string {
	_, t, _ := Probe(pid)
	return t
}
