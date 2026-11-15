//go:build windows

package heartbeat

import (
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func parentName(pid int) string {
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(snapshot)
	if windows.Process32First(snapshot, &entry) != nil {
		return ""
	}
	for {
		if entry.ProcessID == uint32(pid) {
			return boundedRunes(filepath.Base(windows.UTF16ToString(entry.ExeFile[:])), 128)
		}
		if windows.Process32Next(snapshot, &entry) != nil {
			return ""
		}
	}
}
