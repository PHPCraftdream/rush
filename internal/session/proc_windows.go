//go:build windows

package session

import (
	"errors"

	"golang.org/x/sys/windows"
)

// IsProcessAlive is the exported variant of isProcessAlive, used by
// `rush sessions reap` to probe lock holders from the CLI layer.
func IsProcessAlive(pid int) bool { return isProcessAlive(pid) }

func isProcessAlive(pid int) bool {
	return isProcessAliveWithOpenProcess(pid, windows.OpenProcess)
}

// isProcessAliveWithOpenProcess reports whether a process with the given PID is currently
// running. Used to detect orphan locks where the holder crashed without
// releasing — on Windows we open the process with PROCESS_QUERY_LIMITED_
// INFORMATION (cheap, doesn't require admin), then ask for its exit code.
// STILL_ACTIVE (259) means alive; anything else means it has exited.
func isProcessAliveWithOpenProcess(pid int, openProcess func(uint32, bool, uint32) (windows.Handle, error)) bool {
	if pid <= 0 {
		return false
	}
	h, err := openProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
