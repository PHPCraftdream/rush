//go:build windows

package cmd

import (
	"syscall"

	"golang.org/x/sys/windows"
)

var (
	kernel32DLL               = windows.NewLazySystemDLL("kernel32.dll")
	procSetConsoleCtrlHandler = kernel32DLL.NewProc("SetConsoleCtrlHandler")
)

func consoleProcessStopCode(event uint32) (int, bool) {
	switch event {
	case windows.CTRL_C_EVENT, windows.CTRL_BREAK_EVENT,
		windows.CTRL_CLOSE_EVENT, windows.CTRL_LOGOFF_EVENT, windows.CTRL_SHUTDOWN_EVENT:
		return 130, true
	default:
		return 0, false
	}
}

func installConsoleProcessStop(stop func(int)) func() {
	handler := syscall.NewCallback(func(event uint32) uintptr {
		if code, handled := consoleProcessStopCode(event); handled {
			stop(code)
			return 1
		}
		return 0
	})
	if r, _, _ := procSetConsoleCtrlHandler.Call(handler, 1); r == 0 {
		return func() {}
	}
	return func() { _, _, _ = procSetConsoleCtrlHandler.Call(handler, 0) }
}
