//go:build windows

package lab

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func processTestWaitPID(t *testing.T, pid int) {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if !processTestAlive(pid) {
			return
		}
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, 5000)
	if err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("PID %d wait: %d %v", pid, status, err)
	}
	t.Logf("PID %d confirmed dead", pid)
}

func processTestMember(t *testing.T, p *processExecution, pid int) {
	t.Helper()
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	var member int32
	call := windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
	ok, _, err := call.Call(uintptr(handle), uintptr(p.tree.job), uintptr(unsafe.Pointer(&member)))
	if ok == 0 || member == 0 {
		t.Fatalf("PID %d outside execution job: %v", pid, err)
	}
}

func processTestChildren(t *testing.T) map[int]bool {
	t.Helper()
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	parents := map[int]int{}
	helpers := map[int]bool{}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		pid := int(entry.ProcessID)
		parents[pid] = int(entry.ParentProcessID)
		helpers[pid] = windows.UTF16ToString(entry.ExeFile[:]) == "helper.exe"
	}
	result := map[int]bool{}
	for pid := range parents {
		if !helpers[pid] {
			continue
		}
		for parent, steps := parents[pid], 0; parent > 0 && steps < len(parents); parent, steps = parents[parent], steps+1 {
			if parent == os.Getpid() {
				result[pid] = true
				break
			}
		}
	}
	return result
}

func processTestAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var code uint32
	return windows.GetExitCodeProcess(handle, &code) == nil && code == 259
}

func processTestKillPID(pid int) {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err == nil {
		_ = windows.TerminateProcess(handle, 1)
		_ = windows.CloseHandle(handle)
	}
}
