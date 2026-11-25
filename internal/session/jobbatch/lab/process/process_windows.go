//go:build windows

package process

import (
	"fmt"
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type ProcessTree struct{ Job windows.Handle }

func ProcessSupported() bool { return true }
func ProcessPrepare(cmd *exec.Cmd) (*ProcessTree, error) {
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &ProcessTree{Job: job}, nil
}

func (t *ProcessTree) attach(cmd *exec.Cmd) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(t.Job, process); err != nil {
		return err
	}
	// Go closes the primary thread handle; resume the assigned process via NT.
	resume := windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")
	if err := resume.Find(); err != nil {
		return err
	}
	status, _, _ := resume.Call(uintptr(process))
	if status != 0 {
		return fmt.Errorf("resume status %x", status)
	}
	return nil
}
func (t *ProcessTree) term() error { return t.kill() }
func (t *ProcessTree) kill() error { return windows.TerminateJobObject(t.Job, 1) }
func (t *ProcessTree) awaitGone() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var info struct {
			UserTime, KernelTime, PeriodUserTime, PeriodKernelTime           int64
			PageFaults, TotalProcesses, ActiveProcesses, TerminatedProcesses uint32
		}
		err := windows.QueryInformationJobObject(t.Job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
		if err == nil && info.ActiveProcesses == 0 {
			return
		}
		<-ticker.C
	}
}

func ProcessLeaderExited(cmd *exec.Cmd) bool {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && status == windows.WAIT_OBJECT_0
}

func (t *ProcessTree) dispose() error { return windows.CloseHandle(t.Job) }
