//go:build linux

package session

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func startTimeToken(pid int) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false
	}
	content := string(data)
	closeParen := strings.LastIndexByte(content, ')')
	if closeParen < 0 || closeParen+2 > len(content) {
		return "", false
	}
	rest := strings.Fields(content[closeParen+2:])
	const startTimeFieldIndex = 19
	if len(rest) <= startTimeFieldIndex {
		return "", false
	}
	field := rest[startTimeFieldIndex]
	if _, err := strconv.ParseUint(field, 10, 64); err != nil {
		return "", false
	}
	return field, true
}
