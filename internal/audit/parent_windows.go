//go:build windows

package audit

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// parentProcessName returns the parent's executable base name, or "" when unavailable.
func parentProcessName(ppid int) string {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return ""
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	err = windows.Process32First(snap, &entry)
	for err == nil {
		if entry.ProcessID == uint32(ppid) {
			return windows.UTF16ToString(entry.ExeFile[:])
		}
		err = windows.Process32Next(snap, &entry)
	}
	return ""
}
