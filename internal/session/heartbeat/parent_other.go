//go:build !windows

package heartbeat

import (
	"fmt"
	"os"
	"strings"
)

// parentName reads the parent executable name from procfs.
func parentName(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return boundedRunes(strings.TrimSpace(string(b)), 128)
}
