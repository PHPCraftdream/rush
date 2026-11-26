//go:build windows

package machinelock

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"golang.org/x/sys/windows"
)

const stillActive = 259

type windowsProcessValue struct {
	cpu uint64
	io  uint64
}

type windowsTreeState struct {
	last map[int]windowsProcessValue
	gone windowsProcessValue
}

var windowsTreeStates = struct {
	sync.Mutex
	roots map[int]*windowsTreeState
}{roots: make(map[int]*windowsTreeState)}

type processIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

var getProcessIoCounters = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessIoCounters")

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	return code == stillActive
}

func configureProcess(cmd *exec.Cmd) {}

func rootCounterValue(pid int) uint64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	return processCPUValue(h)
}

func processCPUValue(h windows.Handle) uint64 {
	var creation, exit, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &creation, &exit, &kernel, &user) != nil {
		return 0
	}
	kernelTicks := uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)
	userTicks := uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime)
	return kernelTicks + userTicks
}

func processCreationTime(pid int) (uint64, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &creation, &exit, &kernel, &user) != nil {
		return 0, false
	}
	return uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), true
}

func processIOValue(h windows.Handle) uint64 {
	var counters processIOCounters
	r1, _, _ := getProcessIoCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&counters)))
	if r1 == 0 {
		return 0
	}
	return counters.ReadTransferCount + counters.WriteTransferCount + counters.OtherTransferCount
}

func processCurrentValue(pid int) windowsProcessValue {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return windowsProcessValue{}
	}
	defer windows.CloseHandle(h)
	return windowsProcessValue{cpu: processCPUValue(h), io: processIOValue(h)}
}

func processTreePIDs(root int) ([]int, bool) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, false
	}
	defer windows.CloseHandle(snapshot)
	parents := make(map[int]int)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, false
	}
	for {
		parents[int(entry.ProcessID)] = int(entry.ParentProcessID)
		entry.Size = uint32(unsafe.Sizeof(entry))
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if err == windows.ERROR_NO_MORE_FILES {
				break
			}
			return nil, false
		}
	}
	selected := map[int]bool{root: true}
	created := map[int]uint64{}
	createdOK := map[int]bool{}
	if value, ok := processCreationTime(root); ok {
		created[root], createdOK[root] = value, true
	}
	for changed := true; changed; {
		changed = false
		for pid, parent := range parents {
			if selected[pid] || !selected[parent] {
				continue
			}
			parentTime, parentOK := created[parent], createdOK[parent]
			childTime, childOK := processCreationTime(pid)
			created[pid], createdOK[pid] = childTime, childOK
			if parentOK && childOK && childTime < parentTime {
				continue
			}
			selected[pid] = true
			changed = true
		}
	}
	pids := make([]int, 0, len(selected))
	for pid := range selected {
		pids = append(pids, pid)
	}
	return pids, true
}

func processCounters(pid int) uint64 {
	pids, ok := processTreePIDs(pid)
	if !ok {
		return rootCounterValue(pid)
	}
	windowsTreeStates.Lock()
	defer windowsTreeStates.Unlock()
	state := windowsTreeStates.roots[pid]
	if state == nil {
		state = &windowsTreeState{last: make(map[int]windowsProcessValue)}
		windowsTreeStates.roots[pid] = state
	}
	current := make(map[int]bool, len(pids))
	for _, processID := range pids {
		current[processID] = true
		value := processCurrentValue(processID)
		previous := state.last[processID]
		if value.cpu < previous.cpu {
			value.cpu = previous.cpu
		}
		if value.io < previous.io {
			value.io = previous.io
		}
		state.last[processID] = value
	}
	for processID, value := range state.last {
		if !current[processID] {
			state.gone.cpu += value.cpu
			state.gone.io += value.io
			delete(state.last, processID)
		}
	}
	total := state.gone.cpu + state.gone.io
	for _, value := range state.last {
		total += value.cpu + value.io
	}
	return total
}

func processSnapshot(pid int, record Record, started time.Time) string {
	state := "running"
	if !processAlive(pid) {
		state = "exited"
	}
	pids, ok := processTreePIDs(pid)
	if !ok {
		pids = []int{pid}
	}
	return fmt.Sprintf("Tree: PID %d state=%s age=%s command=%q checkout=%q\nWindows process tree: processes=%d aggregated CPU/I/O counter=%d. Child exit codes and thread wait reasons are not implemented on Windows.", pid, state, time.Since(started).Round(time.Second), record.Command, record.Checkout, len(pids), processCounters(pid))
}

func gracefulStop(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = platform.Command(context.Background(), "taskkill", "/PID", fmt.Sprint(cmd.Process.Pid)).Run()
	}
}

func forceStop(cmd *exec.Cmd) {
	if cmd.Process != nil {
		forceStopPID(cmd.Process.Pid)
	}
}

// forceStopPID kills the process tree of pid (also used by tests to clean up a holder).
func forceStopPID(pid int) {
	_ = platform.Command(context.Background(), "taskkill", "/F", "/T", "/PID", fmt.Sprint(pid)).Run()
}
