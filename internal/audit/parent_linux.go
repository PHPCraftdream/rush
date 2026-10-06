//go:build linux

package audit

import (
	"os"
	"strconv"
	"strings"
)

// parentProcessName reads the parent's base name from /proc, or "" when unavailable.
func parentProcessName(ppid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(ppid) + "/comm")
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(b))
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name
}
