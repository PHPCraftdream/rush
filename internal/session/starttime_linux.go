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

// groupMemberPIDs returns the pids of the CURRENT non-leader members of the
// process group pgid, by scanning /proc/*/stat and reading field 5 (pgrp)
// of each stat entry. The parsing mirrors startTimeToken above: comm may
// contain spaces and parens, so everything up to the LAST ')' is skipped
// and the remaining whitespace-separated fields are counted from state
// (field 3) onward -- pgrp is field 5, i.e. rest[2]. The leader itself
// (pid == pgid) is excluded: a witness must be a member that can outlive
// the leader.
//
// Best-effort enumeration: processes that exit mid-scan, or whose stat line
// cannot be parsed, are simply not reported. A partial or empty result is
// the conservative direction everywhere this is used -- fewer witnesses
// means fewer signalled groups (see verifyGroupStillPlausibleOutcome's
// leader-gone branch).
func groupMemberPIDs(pgid int) []int {
	dirEntries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var members []int
	for _, dirEntry := range dirEntries {
		pid, err := strconv.Atoi(dirEntry.Name())
		if err != nil || pid <= 0 || pid == pgid {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		content := string(data)
		closeParen := strings.LastIndexByte(content, ')')
		if closeParen < 0 || closeParen+2 > len(content) {
			continue
		}
		rest := strings.Fields(content[closeParen+2:])
		const pgrpFieldIndex = 2
		if len(rest) <= pgrpFieldIndex {
			continue
		}
		pgrp, err := strconv.Atoi(rest[pgrpFieldIndex])
		if err != nil || pgrp != pgid {
			continue
		}
		members = append(members, pid)
	}
	return members
}
